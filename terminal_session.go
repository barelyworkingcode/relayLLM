package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const (
	terminalScrollbackSize = 100 * 1024 // 100KB
	terminalReadBufSize    = 4096
)

// TerminalSession manages a single PTY-backed terminal process.
type TerminalSession struct {
	ID         string `json:"id"`
	TemplateID string `json:"templateId"`
	Name       string `json:"name"`
	Directory  string `json:"directory"`
	State      string `json:"state"` // "running" or "stopped"
	CreatedAt  string `json:"createdAt"`
	ExitCode   int    `json:"exitCode,omitempty"`

	cols uint16
	rows uint16

	cmd        *exec.Cmd
	ptmx       *os.File // PTY master
	mu         sync.Mutex
	alive      atomic.Bool
	scrollback *scrollBuffer
	waitDone   chan struct{}

	// Idle timeout: kills terminal after no viewers for this duration.
	idleTimeout time.Duration
	idleCancel  chan struct{} // closed to cancel pending idle timer
	idleOnce    sync.Once    // prevents double-close of idleCancel

	onOutput func(terminalID string, data []byte)
	onExit   func(terminalID string, exitCode int)
	onIdle   func(terminalID string) // called when idle timer fires
}

// Start spawns the PTY process for this terminal session.
func (s *TerminalSession) Start(tmpl TerminalTemplate) error {
	command := tmpl.ResolveCommand()

	args := make([]string, len(tmpl.Args))
	copy(args, tmpl.Args)

	cmd := exec.Command(command, args...)
	cmd.Dir = s.Directory
	cmd.Env = ensurePath(os.Environ())
	// Set TERM and COLORTERM for full 24-bit true color support.
	cmd.Env = setEnv(cmd.Env, "TERM", "xterm-256color")
	cmd.Env = setEnv(cmd.Env, "COLORTERM", "truecolor")

	// Merge template-specific env vars.
	for k, v := range tmpl.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	cols := s.cols
	rows := s.rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Rows: rows,
		Cols: cols,
	})
	if err != nil {
		return fmt.Errorf("start pty: %w", err)
	}

	s.cmd = cmd
	s.ptmx = ptmx
	s.alive.Store(true)
	s.State = "running"
	s.scrollback = newScrollBuffer(terminalScrollbackSize)
	s.waitDone = make(chan struct{})

	go s.readLoop()
	go s.waitForExit()

	slog.Info("terminal started", "id", s.ID, "template", s.TemplateID, "command", command, "pid", cmd.Process.Pid)
	return nil
}

func (s *TerminalSession) readLoop() {
	buf := make([]byte, terminalReadBufSize)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])

			s.scrollback.Write(chunk)

			if s.onOutput != nil {
				s.onOutput(s.ID, chunk)
			}
		}
		if err != nil {
			if err != io.EOF {
				slog.Debug("terminal read error", "id", s.ID, "error", err)
			}
			return
		}
	}
}

func (s *TerminalSession) waitForExit() {
	err := s.cmd.Wait()
	s.alive.Store(false)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}

	s.mu.Lock()
	s.State = "stopped"
	s.ExitCode = exitCode
	s.mu.Unlock()

	close(s.waitDone)

	slog.Info("terminal exited", "id", s.ID, "exitCode", exitCode)

	if s.onExit != nil {
		s.onExit(s.ID, exitCode)
	}
}

// Write sends input data to the terminal PTY.
func (s *TerminalSession) Write(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.alive.Load() {
		return fmt.Errorf("terminal not running")
	}

	_, err := s.ptmx.Write(data)
	return err
}

// Resize changes the PTY window size.
func (s *TerminalSession) Resize(cols, rows uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ptmx == nil {
		return fmt.Errorf("terminal not running")
	}

	s.cols = cols
	s.rows = rows
	return pty.Setsize(s.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}

// Close gracefully shuts down the terminal process.
// Sends SIGTERM, waits 3s, then SIGKILL.
func (s *TerminalSession) Close() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}

	s.alive.Store(false)
	s.CancelIdleTimer()

	if s.ptmx != nil {
		s.ptmx.Close()
	}

	_ = s.cmd.Process.Signal(syscall.SIGTERM)

	select {
	case <-s.waitDone:
	case <-time.After(3 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.waitDone
	}

	slog.Info("terminal closed", "id", s.ID)
}

// Alive returns true if the terminal process is still running.
func (s *TerminalSession) Alive() bool {
	return s.alive.Load()
}

// StartIdleTimer begins the idle countdown. If no viewer reconnects before
// it fires, onIdle is called (which should close the terminal).
func (s *TerminalSession) StartIdleTimer() {
	if s.idleTimeout <= 0 || !s.alive.Load() {
		return
	}

	// Cancel any existing timer first.
	s.CancelIdleTimer()

	s.mu.Lock()
	s.idleCancel = make(chan struct{})
	s.idleOnce = sync.Once{}
	cancel := s.idleCancel
	s.mu.Unlock()

	go func() {
		select {
		case <-cancel:
			return
		case <-time.After(s.idleTimeout):
		}
		slog.Info("terminal idle timeout", "id", s.ID, "timeout", s.idleTimeout)
		if s.onIdle != nil {
			s.onIdle(s.ID)
		}
	}()
}

// CancelIdleTimer stops a pending idle timer (e.g. when a viewer reconnects).
func (s *TerminalSession) CancelIdleTimer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleCancel != nil {
		s.idleOnce.Do(func() { close(s.idleCancel) })
	}
}

// Snapshot returns the current state and exit code, safe for concurrent reads.
func (s *TerminalSession) Snapshot() (state string, exitCode int) {
	s.mu.Lock()
	state = s.State
	exitCode = s.ExitCode
	s.mu.Unlock()
	return
}

// ScrollbackBytes returns the current scrollback buffer contents.
func (s *TerminalSession) ScrollbackBytes() []byte {
	if s.scrollback == nil {
		return nil
	}
	return s.scrollback.Bytes()
}

// scrollBuffer is a simple ring buffer for terminal scrollback.
type scrollBuffer struct {
	mu   sync.Mutex
	data []byte
	size int
}

func newScrollBuffer(size int) *scrollBuffer {
	return &scrollBuffer{
		data: make([]byte, 0, size),
		size: size,
	}
}

func (b *scrollBuffer) Write(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.data = append(b.data, p...)
	if len(b.data) > b.size {
		b.data = b.data[len(b.data)-b.size:]
	}
}

func (b *scrollBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, len(b.data))
	copy(out, b.data)
	return out
}

// setEnv sets or replaces an environment variable in a slice.
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
