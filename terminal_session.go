package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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

	// projectID, when set, makes this a project-scoped terminal: relay resolves
	// a project-scoped token for it (validating Directory against the project's
	// path) and the token is injected as RELAY_PROJECT_TOKEN. Empty = ad-hoc
	// terminal, which gets no token.
	projectID string

	// Additional argv tokens appended after the template's resolved Args.
	// Same ${PROJECT_PATH}/${RELAY_TOKEN} substitution applies. Used by
	// scheduled PTY tasks to pass per-task commands into a shared template
	// (e.g. a "shell" template invoked with ["-c","npm test"]).
	extraArgs []string

	cmd        *exec.Cmd
	ptmx       *os.File // PTY master
	mu         sync.Mutex
	alive      atomic.Bool
	scrollback *scrollBuffer
	waitDone   chan struct{}

	// On-disk capture of the raw byte stream. Survives session eviction so
	// the output is replayable after a long-running PTY task exits and the
	// in-memory scrollback is gone. nil disables disk logging.
	logDir string
	logger *terminalLogger

	// Pi project-overlay hooks (set by TerminalManager.SetPiOverlay). Only
	// consulted when the template runs `pi`; non-pi templates ignore them.
	piConfig        *PiConfig
	overlayInputsFn func() PiOverlayInputs

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

	// Resolve relay-managed substitutions before building argv. If the
	// template isn't relay-managed this is a no-op; if relay is unreachable
	// or the project can't be resolved, fail closed (don't spawn with
	// unresolved placeholders).
	subs, err := resolveTemplateSubs(tmpl, s.Directory, s.projectID)
	if err != nil {
		return err
	}

	args := make([]string, 0, len(tmpl.Args)+len(s.extraArgs))
	for _, a := range tmpl.Args {
		args = append(args, subs.Expand(a))
	}
	for _, a := range s.extraArgs {
		args = append(args, subs.Expand(a))
	}

	cmd := exec.Command(command, args...)
	cmd.Dir = s.Directory
	cmd.Env = ensurePath(childBaseEnv())
	// Set TERM and COLORTERM for full 24-bit true color support.
	cmd.Env = setEnv(cmd.Env, "TERM", "xterm-256color")
	cmd.Env = setEnv(cmd.Env, "COLORTERM", "truecolor")

	for k, v := range tmpl.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, subs.Expand(v)))
	}

	// Project-scoped token (empty for ad-hoc terminals); dual-written under the
	// legacy RELAY_TOKEN name for skills that still reference it.
	cmd.Env = setProjectTokenEnv(cmd.Env, subs.RelayToken)
	cmd.Env = applyEnvPassthrough(cmd.Env, tmpl.EnvPassthrough)

	// Pi project-overlay: when the template runs `pi` in a relay-managed
	// project and the overlay is enabled in PiConfig, materialize
	// <projectDir>/.pi/{models,settings,auth}.json and inject
	// PI_CODING_AGENT_DIR. Same hook the LLM provider uses, so PTY and RPC
	// pi sessions see the same models/skills inside the project.
	if isPiCommand(command) && s.piConfig != nil && s.overlayInputsFn != nil {
		var err error
		cmd.Env, err = applyPiOverlayEnv(cmd.Env, s.Directory, s.piConfig, s.overlayInputsFn())
		if err != nil {
			return fmt.Errorf("terminal pi overlay: %w", err)
		}
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

	if s.logDir != "" {
		lg, err := newTerminalLogger(s.logDir, s.ID)
		if err != nil {
			// Disk logging is best-effort: if we can't open files we still
			// let the terminal run, just without replay support.
			slog.Warn("terminal log open failed", "id", s.ID, "error", err)
		} else {
			s.logger = lg
		}
	}

	go s.readLoop()
	go s.waitForExit()

	slog.Info("terminal started", "id", s.ID, "template", s.TemplateID, "command", command, "pid", cmd.Process.Pid)
	return nil
}

func (s *TerminalSession) readLoop() {
	defer s.logger.Close() // nil-safe; flushes and syncs the log files.
	buf := make([]byte, terminalReadBufSize)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])

			s.scrollback.Write(chunk)
			if s.logger != nil {
				s.logger.Write(chunk)
			}

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
	var signalName string
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			// ExitCode == -1 means the process was signaled rather than
			// exiting normally. Surface the signal name in logs so ops can
			// distinguish "process crashed" from "scheduler timed out and
			// killed it".
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				signalName = ws.Signal().String()
			}
		}
	}

	s.mu.Lock()
	s.State = "stopped"
	s.ExitCode = exitCode
	s.mu.Unlock()

	close(s.waitDone)

	if signalName != "" {
		slog.Info("terminal exited", "id", s.ID, "exitCode", exitCode, "signal", signalName)
	} else {
		slog.Info("terminal exited", "id", s.ID, "exitCode", exitCode)
	}

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

// Size returns the current PTY dimensions, defaulting to 80x24 if never set.
func (s *TerminalSession) Size() (cols, rows uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cols, rows = s.cols, s.rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	return
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

// isPiCommand reports whether command's basename is "pi". Used to gate the
// project-overlay only on templates that actually launch the pi CLI, leaving
// shell / claude / opencode templates untouched.
func isPiCommand(command string) bool {
	return command != "" && filepath.Base(command) == "pi"
}

// resolveTemplateSubs is a thin adapter that builds a RelayManagedSpec from
// a TerminalTemplate and delegates to the generic resolver. A non-empty
// projectID makes the terminal project-scoped (gets a token, regardless of the
// template's UseRelayToken flag). See relay_spawn.go.
func resolveTemplateSubs(tmpl TerminalTemplate, directory, projectID string) (SpawnSubs, error) {
	return RelayManagedSpec{
		ProjectID:     projectID,
		Directory:     directory,
		UseRelayToken: tmpl.UseRelayToken,
		Label:         "template " + tmpl.ID,
	}.Resolve()
}
