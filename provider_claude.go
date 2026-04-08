package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const claudeIdleTimeout = 15 * time.Minute

// ClaudeProvider manages a persistent Claude CLI process.
type ClaudeProvider struct {
	session *Session
	handler EventHandler

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	mu     sync.Mutex // serializes writes to stdin
	alive  atomic.Bool

	claudeSessionID string
	model           string
	directory       string
	hookURL         string // URL for permission hook binary

	lastActivity atomic.Int64  // unix timestamp of last activity
	stopIdle     chan struct{} // signals idle watcher to stop
	stopIdleOnce sync.Once    // prevents double-close of stopIdle
	waitDone     chan struct{} // closed when cmd.Wait() returns

	// Per-turn timing for TTFT / TPS metrics.
	msgStartNano   atomic.Int64 // set in SendMessage
	firstTokenNano atomic.Int64 // set on first content_block_delta
}

func NewClaudeProvider(session *Session, handler EventHandler, hookURL string) *ClaudeProvider {
	return &ClaudeProvider{
		session:   session,
		handler:   handler,
		model:     session.Model,
		directory: session.Directory,
		hookURL:   hookURL,
	}
}

func (p *ClaudeProvider) touchActivity() {
	p.lastActivity.Store(time.Now().Unix())
}

func (p *ClaudeProvider) idleWatcher() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopIdle:
			return
		case <-ticker.C:
			idle := time.Now().Unix() - p.lastActivity.Load()
			if idle > int64(claudeIdleTimeout.Seconds()) {
				slog.Info("claude process idle, killing", "session", p.session.ID, "idleSecs", idle)
				p.Kill()
				return
			}
		}
	}
}

func (p *ClaudeProvider) Start() error {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--model", p.model,
	}

	if p.claudeSessionID != "" {
		args = append(args, "--resume", p.claudeSessionID)
	}

	if p.session.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", p.session.SystemPrompt)
	}

	var settings struct {
		Headless bool `json:"headless"`
	}
	if p.session.Settings != nil {
		json.Unmarshal(p.session.Settings, &settings)
	}
	if settings.Headless {
		args = append(args, "--dangerously-skip-permissions", "--permission-mode", "bypassPermissions")
	}

	claudePath := resolveClaudePath()
	cmd := exec.Command(claudePath, args...)
	cmd.Dir = p.directory
	cmd.Env = ensurePath(os.Environ())
	cmd.Env = append(cmd.Env,
		fmt.Sprintf("RELAY_LLM_HOOK_URL=%s", p.hookURL),
		fmt.Sprintf("RELAY_LLM_SESSION_ID=%s", p.session.ID),
	)

	if settings.Headless {
		cmd.Env = append(cmd.Env, "RELAY_LLM_HEADLESS=true")
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start claude: %w", err)
	}

	p.cmd = cmd
	p.stdin = stdin
	p.alive.Store(true)
	p.stopIdle = make(chan struct{})
	p.stopIdleOnce = sync.Once{}
	p.waitDone = make(chan struct{})
	p.touchActivity()

	go p.readStdout(stdout)
	go p.readStderr(stderr)
	go p.waitForExit()
	go p.idleWatcher()

	slog.Info("claude process started", "session", p.session.ID, "model", p.model, "pid", cmd.Process.Pid)
	return nil
}

func (p *ClaudeProvider) readStdout(r io.ReadCloser) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		p.processLine(json.RawMessage(append([]byte(nil), line...)))
	}

	if err := scanner.Err(); err != nil {
		slog.Error("claude stdout read error", "session", p.session.ID, "error", err)
	}
}

func (p *ClaudeProvider) readStderr(r io.ReadCloser) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		text := scanner.Text()
		if text != "" {
			slog.Debug("claude stderr", "session", p.session.ID, "text", text)
		}
	}
}

func (p *ClaudeProvider) waitForExit() {
	err := p.cmd.Wait()
	p.alive.Store(false)
	close(p.waitDone)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}

	slog.Info("claude process exited", "session", p.session.ID, "exitCode", exitCode)

	data, _ := json.Marshal(map[string]interface{}{
		"exitCode": exitCode,
	})
	p.handler("process_exited", data)
}

func (p *ClaudeProvider) processLine(raw json.RawMessage) {
	p.touchActivity()

	// Extract the event type for routing.
	var envelope struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype,omitempty"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		// Non-JSON output — forward as raw_output.
		p.handler("raw_output", raw)
		return
	}

	// Record first-token time on the first content_block_delta.
	if envelope.Type == "content_block_delta" {
		p.firstTokenNano.CompareAndSwap(0, time.Now().UnixNano())
	}

	// Capture Claude session ID from system.init.
	if envelope.Type == "system" && envelope.Subtype == "init" {
		var init struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(raw, &init); err == nil && init.SessionID != "" {
			p.mu.Lock()
			p.claudeSessionID = init.SessionID
			p.mu.Unlock()
		}
	}

	// Extract stats from result events.
	if envelope.Type == "result" {
		var result struct {
			Usage *struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
			TotalCostUsd float64 `json:"total_cost_usd"`
		}
		if err := json.Unmarshal(raw, &result); err == nil && result.Usage != nil {
			stats := SessionStats{
				InputTokens:         result.Usage.InputTokens,
				OutputTokens:        result.Usage.OutputTokens,
				CacheReadTokens:     result.Usage.CacheReadInputTokens,
				CacheCreationTokens: result.Usage.CacheCreationInputTokens,
				CostUsd:             result.TotalCostUsd,
			}

			startNano := p.msgStartNano.Load()
			firstNano := p.firstTokenNano.Load()
			nowNano := time.Now().UnixNano()
			if startNano > 0 && firstNano > 0 {
				stats.TimeToFirstToken = float64(firstNano-startNano) / 1e9
				genSecs := float64(nowNano-firstNano) / 1e9
				if genSecs > 0 && stats.OutputTokens > 0 {
					stats.TokensPerSecond = float64(stats.OutputTokens) / genSecs
				}
			}

			statsData, _ := json.Marshal(stats)
			p.handler("stats_update", statsData)
		}
		p.handler("message_complete", nil)
	}

	// Forward all events as llm_event.
	p.handler("llm_event", raw)
}

func (p *ClaudeProvider) SendMessage(text string, files []FileAttachment) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.alive.Load() || p.stdin == nil {
		return fmt.Errorf("claude process not running")
	}

	p.touchActivity()

	// Build content blocks.
	var content []interface{}
	for _, f := range files {
		content = append(content, map[string]interface{}{
			"type": "image",
			"source": map[string]string{
				"type":       "base64",
				"media_type": f.MimeType,
				"data":       f.Data,
			},
		})
	}
	content = append(content, map[string]string{
		"type": "text",
		"text": text,
	})

	msg := map[string]interface{}{
		"type": "user",
		"message": map[string]interface{}{
			"role":    "user",
			"content": content,
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	data = append(data, '\n')

	// Record turn start for TTFT/TPS calculation.
	p.msgStartNano.Store(time.Now().UnixNano())
	p.firstTokenNano.Store(0)

	if _, err := p.stdin.Write(data); err != nil {
		return fmt.Errorf("write to stdin: %w", err)
	}

	return nil
}

func (p *ClaudeProvider) StopGeneration() {
	// Claude CLI doesn't have a lightweight stop — Kill is the only option.
	p.Kill()
}

func (p *ClaudeProvider) Kill() {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}

	// Mark dead early so concurrent SendMessage calls fail fast
	// instead of writing to a closing stdin pipe.
	p.alive.Store(false)

	// Stop the idle watcher goroutine.
	if p.stopIdle != nil {
		p.stopIdleOnce.Do(func() { close(p.stopIdle) })
	}

	if p.stdin != nil {
		p.stdin.Close()
	}

	// Try SIGTERM first, then SIGKILL after 3 seconds.
	_ = p.cmd.Process.Signal(os.Interrupt)

	select {
	case <-p.waitDone:
	case <-time.After(3 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.waitDone
	}

	slog.Info("claude process killed", "session", p.session.ID)
}

func (p *ClaudeProvider) Alive() bool {
	return p.alive.Load()
}

func (p *ClaudeProvider) DeleteSession() error {
	p.mu.Lock()
	sid := p.claudeSessionID
	p.mu.Unlock()
	if sid == "" {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}

	pattern := filepath.Join(home, ".claude", "projects", "*", sid+".jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob claude session: %w", err)
	}

	for _, path := range matches {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove claude session file: %w", err)
		}
		slog.Info("deleted claude session file", "path", path)
	}

	return nil
}

func (p *ClaudeProvider) GetState() json.RawMessage {
	p.mu.Lock()
	sid := p.claudeSessionID
	p.mu.Unlock()
	state := map[string]interface{}{
		"claudeSessionId": sid,
	}
	data, _ := json.Marshal(state)
	return data
}

func (p *ClaudeProvider) RestoreState(state json.RawMessage) {
	if state == nil {
		return
	}
	var s struct {
		ClaudeSessionID string `json:"claudeSessionId"`
	}
	if err := json.Unmarshal(state, &s); err == nil {
		p.mu.Lock()
		p.claudeSessionID = s.ClaudeSessionID
		p.mu.Unlock()
	}
}

// resolveClaudePath finds the claude binary, checking well-known locations
// before falling back to PATH lookup. Necessary when launched from minimal
// environments (Raycast, launchd) that don't source shell profiles.
func resolveClaudePath() string {
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", "claude"),
		filepath.Join(home, ".claude", "local", "claude"),
		"/usr/local/bin/claude",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// Fall back to PATH lookup.
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	return "claude"
}

// ensurePath adds ~/.local/bin to PATH in the environment slice if not already present.
func ensurePath(env []string) []string {
	home, _ := os.UserHomeDir()
	localBin := filepath.Join(home, ".local", "bin")

	for i, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			if !strings.Contains(e, localBin) {
				env[i] = e + ":" + localBin
			}
			return env
		}
	}
	// No PATH at all — set one.
	return append(env, "PATH=/usr/local/bin:/usr/bin:/bin:"+localBin)
}
