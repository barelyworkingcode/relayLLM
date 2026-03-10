package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

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

	cmd := exec.Command("claude", args...)
	cmd.Dir = p.directory
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("RELAY_LLM_HOOK_URL=%s", p.hookURL),
		fmt.Sprintf("RELAY_LLM_SESSION_ID=%s", p.session.ID),
	)

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

	go p.readStdout(stdout)
	go p.readStderr(stderr)
	go p.waitForExit()

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
	// Extract the event type for routing.
	var envelope struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype,omitempty"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		// Non-JSON output — forward as raw_output.
		data, _ := json.Marshal(map[string]string{"text": string(raw)})
		p.handler("raw_output", data)
		return
	}

	// Capture Claude session ID from system.init.
	if envelope.Type == "system" && envelope.Subtype == "init" {
		var init struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(raw, &init); err == nil && init.SessionID != "" {
			p.claudeSessionID = init.SessionID
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

	if _, err := p.stdin.Write(data); err != nil {
		return fmt.Errorf("write to stdin: %w", err)
	}

	return nil
}

func (p *ClaudeProvider) Kill() {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}

	if p.stdin != nil {
		p.stdin.Close()
	}

	// Try SIGTERM first, then SIGKILL after 3 seconds.
	_ = p.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() {
		p.cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
	}

	p.alive.Store(false)
	slog.Info("claude process killed", "session", p.session.ID)
}

func (p *ClaudeProvider) Alive() bool {
	return p.alive.Load()
}

func (p *ClaudeProvider) GetState() json.RawMessage {
	state := map[string]interface{}{
		"claudeSessionId": p.claudeSessionID,
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
		p.claudeSessionID = s.ClaudeSessionID
	}
}
