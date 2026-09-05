package main

import (
	"bufio"
	"bytes"
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

var emptyJSONObject = []byte("{}")

const claudeIdleTimeout = 15 * time.Minute

// ClaudeProvider manages a persistent Claude CLI process and translates its
// stream-json output into canonical relay events.
//
// One legacy variant gets normalized here so clients see only the canonical
// shape: a content_block_start for a text or thinking block that carries
// inline content is split into bare-start + delta.
//
// Persistence: Claude CLI owns the JSONL history file under ~/.claude/
// projects/<dir>/<sid>.jsonl, replayed on session join by readClaudeHistory.
// This provider does not accumulate into session.Messages.
type ClaudeProvider struct {
	session *Session
	handler EventHandler
	emitter *EventEmitter

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	mu     sync.Mutex // serializes writes to stdin
	alive  atomic.Bool

	claudeSessionID string
	model           string
	directory       string
	hookSocket      string // Unix socket path the hook subprocess dials for /api/permission
	hookToken       string // bearer token the hook will send when calling /api/permission

	// perms services a host session's control_request permission prompts
	// (../relay/docs/ssh-hosts.md decision 5). nil-safe: a provider built
	// without one (existing tests, non-host sessions with no policy match)
	// simply can't register/deny requests — control_request handling checks
	// before use.
	perms *PermissionManager

	lastActivity atomic.Int64  // unix timestamp of last activity
	stopIdle     chan struct{} // signals idle watcher to stop
	stopIdleOnce sync.Once     // prevents double-close of stopIdle
	waitDone     chan struct{} // closed when cmd.Wait() returns

	msgStartNano   atomic.Int64
	firstTokenNano atomic.Int64

	// Per-turn snapshot state. Claude CLI 2.1.x ships one `assistant` event
	// per fully-completed content block (each event's message.content[]
	// carries the just-finalized block, not a cumulative snapshot). Events
	// sharing the same message.id belong to one turn. We allocate monotonic
	// global block indices across events so block N+1's start/delta/stop
	// don't collide with block N.
	snapMu        sync.Mutex
	snapMessageID string // message.id of the in-progress turn
	snapNextIdx   int    // next global block index to assign
}

func NewClaudeProvider(session *Session, handler EventHandler, hookSocket, hookToken string, perms *PermissionManager) *ClaudeProvider {
	return &ClaudeProvider{
		session:    session,
		handler:    handler,
		emitter:    NewEventEmitter(handler),
		model:      session.Model,
		directory:  session.Directory,
		hookSocket: hookSocket,
		hookToken:  hookToken,
		perms:      perms,
	}
}

// relayMCPConfigJSON renders the shared relay MCP server entry as the
// inline JSON Claude CLI's --mcp-config expects. Returns "" when the
// session hasn't opted in or relayLLM wasn't started under relay.
func (p *ClaudeProvider) relayMCPConfigJSON(projectToken string) string {
	settings := parseBaseSettings(p.session.Settings)
	if settings.UseRelayTools == nil || !*settings.UseRelayTools {
		return ""
	}
	relay, ok := resolveRelayMCPServer(projectToken)
	if !ok {
		return ""
	}
	cfg := map[string]any{
		"mcpServers": map[string]any{"relay": relay},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		slog.Warn("claude: marshal relay MCP config failed", "error", err)
		return ""
	}
	return string(data)
}

// resolveMCPToken returns the project-scoped relay token for this session,
// used for both Claude's own RELAY_PROJECT_TOKEN env and the --mcp-config
// child. It resolves just-in-time from relay's bridge by project id — relay is
// the sole authority, so this is restart- and rotation-safe and never depends
// on a stored/eve-supplied token. Returns "" when not relay-managed; callers
// degrade (they never fall back to the full-access service token).
func (p *ClaudeProvider) resolveMCPToken() string {
	return resolveProjectToken(p.session)
}

// refreshHostSpec re-resolves the session's Host via relay's bridge before
// each spawn, so a probe update (new claude_path, host record edit) takes
// effect on the next turn. On any failure — including a standalone run with
// no service token — it leaves the already-stored Host untouched: a Host is
// a routing fact, not a credential, so falling back to the stored value (not
// clearing it) is what lets a persisted host session resume after a
// relayLLM restart while relay is briefly unreachable.
func (p *ClaudeProvider) refreshHostSpec() {
	if serviceToken() == "" {
		return
	}
	resp, err := resolveRelayPtyEnv(RelayPtyEnvRequest{ProjectID: p.session.ProjectID, Directory: p.directory})
	if err != nil {
		slog.Warn("resolve host at spawn failed, using stored value", "session", p.session.ID, "error", err)
		return
	}
	p.session.setHost(resp.Host)
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

// effectivePermissionMode resolves the Claude CLI --permission-mode for this
// session. A headless session forces "bypassPermissions" — the legacy synonym
// takes precedence even if a different mode is set. This single source of truth
// is what gates both --dangerously-skip-permissions and RELAY_LLM_HEADLESS, so
// the headless escape hatch can never be half-applied.
func (p *ClaudeProvider) effectivePermissionMode() string {
	if p.session.Headless {
		return "bypassPermissions"
	}
	return p.session.PermissionMode
}

// buildClaudeArgs assembles the claude CLI argv. mcpCfg is the rendered
// --mcp-config JSON ("" to omit). Pure over provider fields + the one argument,
// so the security-sensitive flag matrix (resume, permission mode, the
// --dangerously-skip-permissions escape hatch, policy tools) is hermetically
// testable without spawning a process. See provider_claude_spawn_test.go and
// the headless-isolation guard in security_regression_test.go.
func (p *ClaudeProvider) buildClaudeArgs(mcpCfg string) []string {
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

	mode := p.effectivePermissionMode()
	if mode != "" && mode != "default" {
		args = append(args, "--permission-mode", mode)
	}
	if mode == "bypassPermissions" {
		args = append(args, "--dangerously-skip-permissions")
	}

	if p.session.Policy != nil {
		if len(p.session.Policy.AllowedTools) > 0 {
			args = append(args, "--allowedTools", strings.Join(p.session.Policy.AllowedTools, ","))
		}
		if len(p.session.Policy.DeniedTools) > 0 {
			args = append(args, "--disallowedTools", strings.Join(p.session.Policy.DeniedTools, ","))
		}
	}

	// A host session has no PreToolUse hook binary or bridge socket to dial
	// (neither exists on the host), so permissions ride the stream instead:
	// --permission-prompt-tool stdio turns each tool call into a
	// control_request on Claude's own stdout (see processLine). --mcp-config
	// is never passed on a host — v1 carries no relay MCPs there (decision 6).
	if p.session.Host != nil {
		args = append(args, "--permission-prompt-tool", "stdio")
	} else if mcpCfg != "" {
		args = append(args, "--mcp-config", mcpCfg)
	}

	return args
}

// buildClaudeEnv assembles the child environment. base is the inherited
// environment (childBaseEnv after ensurePath); mcpToken is the resolved
// project-scoped relay token — "" when the session is not relay-managed.
//
// Fail-closed contract: an empty mcpToken sets NO project-token var (never the
// full-access service token — see setProjectTokenEnv). RELAY_LLM_HEADLESS is
// set if and only if the effective permission mode is bypassPermissions, so the
// hook's auto-approve only ever fires for a session that is actually headless.
func (p *ClaudeProvider) buildClaudeEnv(base []string, mcpToken string) []string {
	env := append(base,
		fmt.Sprintf("RELAY_LLM_HOOK_SOCKET=%s", p.hookSocket),
		fmt.Sprintf("RELAY_LLM_SESSION_ID=%s", p.session.ID),
	)
	if p.hookToken != "" {
		env = append(env, fmt.Sprintf("RELAY_LLM_HOOK_TOKEN=%s", p.hookToken))
	}

	// Expose the project-scoped relay token to Claude itself, not just to
	// the --mcp-config child. Project skills (CLAUDE.md) commonly tell the
	// model to invoke `relay mcp call ...` via Bash; that path needs
	// RELAY_PROJECT_TOKEN in Claude's own environment to authenticate.
	// Dual-written under the legacy RELAY_TOKEN name for existing skills.
	env = setProjectTokenEnv(env, mcpToken)

	if p.effectivePermissionMode() == "bypassPermissions" {
		env = append(env, "RELAY_LLM_HEADLESS=true")
	}
	return env
}

// buildHostExec assembles argv to run Claude on a host: relay's ssh_argv
// prefix, `-T` (no local tty — this is the headless chat process, not a
// terminal), `--`, and RemoteCommand's base64 launcher wrapping
// `claude_path <args>` under dir with env. Pure over its inputs (ADR-008): no
// bridge call, no exec, hermetically testable. env is caller-built so the
// security invariant — RELAY_LLM_SESSION_ID only, never the hook socket/token
// or any relay token (decision 6) — is visible at the call site too.
func buildHostExec(spec *HostSpec, dir string, args []string, env map[string]string) (name string, argv []string) {
	remote := RemoteCommand(dir, append([]string{spec.ClaudePath}, args...), env)
	name = spec.SSHArgv[0]
	argv = append(append([]string{}, spec.SSHArgv[1:]...), "-T", "--", remote)
	return name, argv
}

func (p *ClaudeProvider) Start() error {
	p.refreshHostSpec()

	var cmd *exec.Cmd
	if host := p.session.getHost(); host != nil {
		if host.ClaudePath == "" {
			return fmt.Errorf("host %q has no claude: run a probe", host.Name)
		}
		args := p.buildClaudeArgs("")
		env := map[string]string{"RELAY_LLM_SESSION_ID": p.session.ID}
		name, argv := buildHostExec(host, p.directory, args, env)
		cmd = exec.Command(name, argv...)
		// cmd.Dir stays the console's cwd — irrelevant, since the working
		// directory that matters (dir) is applied by RemoteCommand's `cd` on
		// the host side.
		cmd.Env = childBaseEnv()
	} else {
		// Resolve the relay project token once and reuse it for both the
		// --mcp-config child and Claude's own env. Resilient to a relayLLM
		// restart, which drops the non-persisted session.McpToken.
		mcpToken := p.resolveMCPToken()
		mcpCfg := p.relayMCPConfigJSON(mcpToken)

		args := p.buildClaudeArgs(mcpCfg)

		claudePath := resolveClaudePath()
		cmd = exec.Command(claudePath, args...)
		cmd.Dir = p.directory
		cmd.Env = p.buildClaudeEnv(ensurePath(childBaseEnv()), mcpToken)
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

	var envelope struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype,omitempty"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		p.handler("raw_output", raw)
		return
	}

	switch envelope.Type {
	case EvtSystem:
		p.translateSystem(envelope.Subtype, raw)
	case EvtAssistant:
		p.translateAssistant(raw)
	case "user":
		p.translateUser(raw)
	case EvtResult:
		p.translateResult(raw)
	case "control_request":
		p.handleControlRequest(raw)
	default:
		// Claude-CLI-specific events outside the canonical trio
		// (permission-mode, ai-title, custom-title). Forward with v stamped.
		p.emitter.EmitVersionedRaw(raw)
	}
}

// claudeMCPServerNames extracts the `name` field from each entry in
// mcp_servers. Tolerates both the legacy []string shape (Claude CLI <=2.0,
// raw is a bare JSON string) and the 2.1.x []{name,status,...} shape
// (raw is a JSON object); silently drops entries with neither.
func claudeMCPServerNames(entries []json.RawMessage) []string {
	names := make([]string, 0, len(entries))
	for _, raw := range entries {
		if len(raw) == 0 {
			continue
		}
		if raw[0] == '"' {
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				names = append(names, s)
			}
			continue
		}
		var obj struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &obj) == nil && obj.Name != "" {
			names = append(names, obj.Name)
		}
	}
	return names
}

func (p *ClaudeProvider) translateSystem(subtype string, raw json.RawMessage) {
	switch subtype {
	case SystemInitSubtype:
		var init struct {
			SessionID  string            `json:"session_id"`
			Model      string            `json:"model"`
			Cwd        string            `json:"cwd"`
			Tools      []string          `json:"tools"`
			MCPServers []json.RawMessage `json:"mcp_servers"`
		}
		if err := json.Unmarshal(raw, &init); err != nil {
			p.emitter.EmitVersionedRaw(raw)
			return
		}
		if init.SessionID != "" {
			p.mu.Lock()
			p.claudeSessionID = init.SessionID
			p.mu.Unlock()
		}
		model := init.Model
		if model == "" {
			model = p.model
		}
		cwd := init.Cwd
		if cwd == "" {
			cwd = p.directory
		}
		p.emitter.SystemInit(model, cwd, init.Tools, claudeMCPServerNames(init.MCPServers))

	case SystemPermissionRequestSubtype:
		var req struct {
			PermissionID string          `json:"permission_id"`
			ToolName     string          `json:"tool_name"`
			ToolUseID    string          `json:"tool_use_id"`
			ToolInput    json.RawMessage `json:"tool_input"`
		}
		if json.Unmarshal(raw, &req) != nil {
			p.emitter.EmitVersionedRaw(raw)
			return
		}
		p.emitter.PermissionRequest(req.PermissionID, req.ToolName, req.ToolUseID, req.ToolInput)

	case SystemQuestionSubtype:
		var q struct {
			Prompt   string          `json:"prompt"`
			Metadata json.RawMessage `json:"metadata"`
		}
		if json.Unmarshal(raw, &q) != nil {
			p.emitter.EmitVersionedRaw(raw)
			return
		}
		p.emitter.SystemQuestion(q.Prompt, q.Metadata)

	case SystemStatusSubtype:
		var s struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &s) != nil {
			p.emitter.EmitVersionedRaw(raw)
			return
		}
		p.emitter.SystemStatus(s.Message)

	case SystemAPIErrorSubtype:
		var s struct {
			Message  string `json:"message"`
			Retrying bool   `json:"retrying"`
		}
		if json.Unmarshal(raw, &s) != nil {
			p.emitter.EmitVersionedRaw(raw)
			return
		}
		p.emitter.SystemAPIError(s.Message, s.Retrying)

	case SystemBridgeStatusSubtype:
		var s struct {
			Status string `json:"status"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(raw, &s) != nil {
			p.emitter.EmitVersionedRaw(raw)
			return
		}
		p.emitter.SystemBridgeStatus(s.Status, s.Detail)

	case SystemStopHookSummarySubtype:
		var s struct {
			Summary string `json:"summary"`
			IsError bool   `json:"is_error"`
		}
		if json.Unmarshal(raw, &s) != nil {
			p.emitter.EmitVersionedRaw(raw)
			return
		}
		p.emitter.SystemStopHookSummary(s.Summary, s.IsError)

	default:
		p.emitter.EmitVersionedRaw(raw)
	}
}

func (p *ClaudeProvider) translateAssistant(raw json.RawMessage) {
	var ev struct {
		Index            *int             `json:"index,omitempty"`
		Message          *json.RawMessage `json:"message,omitempty"`
		ContentBlock     *json.RawMessage `json:"content_block,omitempty"`
		Delta            *json.RawMessage `json:"delta,omitempty"`
		ContentBlockStop *bool            `json:"content_block_stop,omitempty"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		p.emitter.EmitVersionedRaw(raw)
		return
	}

	isStop := ev.ContentBlockStop != nil && *ev.ContentBlockStop

	switch {
	case ev.Message != nil:
		// Claude CLI 2.1.x ships full-message snapshots here; older CLIs
		// shipped a bare `message_start` with no content. Both are routed
		// through translateAssistantSnapshot, which is a no-op for empty
		// content arrays.
		p.translateAssistantSnapshot(*ev.Message)

	case ev.Delta != nil && ev.Index != nil:
		p.firstTokenNano.CompareAndSwap(0, time.Now().UnixNano())
		p.translateBlockDelta(*ev.Index, *ev.Delta)

	case isStop && ev.Index != nil:
		p.translateBlockStop(*ev.Index, ev.ContentBlock)

	case !isStop && ev.ContentBlock != nil && ev.Index != nil:
		p.translateBlockStart(*ev.Index, *ev.ContentBlock)

	default:
		// message_delta / message_stop and anything else without enough info
		// to type — result event carries the final state we care about.
		p.emitter.EmitVersionedRaw(raw)
	}
}

// claudeSnapContentBlock mirrors the fields we read out of a snapshot's
// message.content[] entry. Claude CLI puts the streaming-style block kind
// and full accumulated payload directly on each entry.
type claudeSnapContentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Thinking string          `json:"thinking,omitempty"`
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
}

// translateAssistantSnapshot processes one `assistant` event from Claude
// CLI 2.1.x. Each event's message.content[] holds fully-completed block(s)
// for the current turn (identified by message.id), so we emit
// start/delta/stop in one shot per block and allocate a fresh global index
// per content[] entry across events. A change in message.id resets the
// per-turn index and emits a new MessageStart.
func (p *ClaudeProvider) translateAssistantSnapshot(messageRaw json.RawMessage) {
	var m struct {
		ID      string                   `json:"id"`
		Content []claudeSnapContentBlock `json:"content"`
	}
	if err := json.Unmarshal(messageRaw, &m); err != nil {
		p.emitter.EmitVersionedRaw(messageRaw)
		return
	}

	p.snapMu.Lock()
	defer p.snapMu.Unlock()

	if m.ID != p.snapMessageID {
		p.snapMessageID = m.ID
		p.snapNextIdx = 0
		p.emitter.MessageStart(m.ID)
	}

	for _, block := range m.Content {
		idx := p.snapNextIdx
		p.snapNextIdx++
		switch block.Type {
		case BlockText:
			p.emitter.TextBlockStart(idx)
			if block.Text != "" {
				p.firstTokenNano.CompareAndSwap(0, time.Now().UnixNano())
				p.emitter.TextDelta(idx, block.Text)
			}
			p.emitter.BlockStop(idx)
		case BlockThinking:
			p.emitter.ThinkingBlockStart(idx)
			if block.Thinking != "" {
				p.firstTokenNano.CompareAndSwap(0, time.Now().UnixNano())
				p.emitter.ThinkingDelta(idx, block.Thinking)
			}
			p.emitter.BlockStop(idx)
		case BlockToolUse:
			p.emitter.ToolUseBlockStart(idx, block.ID, block.Name)
			input := block.Input
			if len(input) > 0 && !bytes.Equal(input, emptyJSONObject) {
				p.emitter.InputJsonDelta(idx, string(input))
			}
			p.emitter.ToolUseBlockStop(idx, block.ID, block.Name, input)
		}
	}
}

// translateBlockStart emits content_block_start, normalizing the legacy variant
// where a text/thinking start carries inline content (split into start + delta).
func (p *ClaudeProvider) translateBlockStart(index int, blockRaw json.RawMessage) {
	var cb struct {
		Type     string          `json:"type"`
		ID       string          `json:"id,omitempty"`
		Name     string          `json:"name,omitempty"`
		Text     string          `json:"text,omitempty"`
		Thinking string          `json:"thinking,omitempty"`
		Input    json.RawMessage `json:"input,omitempty"`
	}
	if err := json.Unmarshal(blockRaw, &cb); err != nil {
		return
	}

	switch cb.Type {
	case BlockText:
		p.emitter.TextBlockStart(index)
		if cb.Text != "" {
			p.emitter.TextDelta(index, cb.Text)
		}
	case BlockThinking:
		p.emitter.ThinkingBlockStart(index)
		if cb.Thinking != "" {
			p.emitter.ThinkingDelta(index, cb.Thinking)
		}
	case BlockToolUse:
		p.emitter.ToolUseBlockStart(index, cb.ID, cb.Name)
		if len(cb.Input) > 0 && string(cb.Input) != "{}" {
			p.emitter.InputJsonDelta(index, string(cb.Input))
		}
	}
}

func (p *ClaudeProvider) translateBlockDelta(index int, deltaRaw json.RawMessage) {
	var d struct {
		Type        string `json:"type"`
		Text        string `json:"text,omitempty"`
		Thinking    string `json:"thinking,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
	}
	if err := json.Unmarshal(deltaRaw, &d); err != nil {
		return
	}
	switch d.Type {
	case DeltaText:
		p.emitter.TextDelta(index, d.Text)
	case DeltaThinking:
		p.emitter.ThinkingDelta(index, d.Thinking)
	case DeltaInputJSON:
		p.emitter.InputJsonDelta(index, d.PartialJSON)
	}
}

func (p *ClaudeProvider) translateBlockStop(index int, blockRaw *json.RawMessage) {
	if blockRaw == nil {
		p.emitter.BlockStop(index)
		return
	}
	var cb struct {
		Type  string          `json:"type"`
		ID    string          `json:"id,omitempty"`
		Name  string          `json:"name,omitempty"`
		Input json.RawMessage `json:"input,omitempty"`
	}
	if err := json.Unmarshal(*blockRaw, &cb); err != nil || cb.Type != BlockToolUse {
		p.emitter.BlockStop(index)
		return
	}
	p.emitter.ToolUseBlockStop(index, cb.ID, cb.Name, cb.Input)
}

// translateUser handles Claude CLI's `user` events, which carry tool_result
// content blocks. Each tool_result inside the user message is emitted as a
// canonical result.tool_result event paired by tool_use_id.
func (p *ClaudeProvider) translateUser(raw json.RawMessage) {
	var ev struct {
		Message struct {
			Content []struct {
				Type      string          `json:"type"`
				ToolUseID string          `json:"tool_use_id"`
				ToolName  string          `json:"tool_name,omitempty"`
				Content   json.RawMessage `json:"content"`
				IsError   bool            `json:"is_error"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		p.emitter.EmitVersionedRaw(raw)
		return
	}

	for _, block := range ev.Message.Content {
		if block.Type != "tool_result" {
			continue
		}
		p.emitter.ToolResult(block.ToolUseID, block.ToolName, flattenTextBlocks(block.Content), block.IsError)
	}
}

// translateResult handles the terminal `result` event: extracts usage into
// SessionStats, emits stats_update, then emits message_complete with nil data
// (Claude CLI owns assistant persistence via its JSONL file — the session
// layer's fallback-save branch must not fire).
func (p *ClaudeProvider) translateResult(raw json.RawMessage) {
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
		p.handler(HandlerStatsUpdate, statsData)
	}
	p.handler(HandlerMessageComplete, nil)
}

// ---------------------------------------------------------------------------
// control_request — host session permissions (../relay/docs/ssh-hosts.md
// decision 5). A host has no PreToolUse hook binary or bridge socket, so
// --permission-prompt-tool stdio moves the same question onto Claude's own
// stdout as a control_request and takes the answer on stdin as a
// control_response. Console sessions never emit control_request (they keep
// the hook), so this is a no-op branch for them.
// ---------------------------------------------------------------------------

// claudeControlRequest mirrors the subset of Claude Agent SDK's
// control_request envelope this provider understands. RequestID is kept as
// raw JSON (not decoded to a Go number) so it can be echoed back byte-for-byte
// in the response regardless of whether Claude sent it as a string or number.
type claudeControlRequest struct {
	RequestID json.RawMessage `json:"request_id"`
	Request   struct {
		Subtype   string          `json:"subtype"`
		ToolName  string          `json:"tool_name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
	} `json:"request"`
}

// controlResponseResult is the inner decision object of a successful
// control_response — the shape --permission-prompt-tool stdio expects back.
type controlResponseResult struct {
	Behavior     string          `json:"behavior"`
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
	Message      string          `json:"message,omitempty"`
}

type controlResponseBody struct {
	RequestID json.RawMessage         `json:"request_id"`
	Subtype   string                  `json:"subtype"`
	Response  *controlResponseResult  `json:"response,omitempty"`
	Error     string                  `json:"error,omitempty"`
}

type controlResponseEnvelope struct {
	Type     string               `json:"type"`
	Response controlResponseBody  `json:"response"`
}

// buildControlResponseAllow builds the exact allow control_response bytes
// pinned in ../relay/docs/ssh-hosts.md. An empty input is normalized to "{}"
// so updatedInput is always a JSON object, never absent or null.
func buildControlResponseAllow(requestID, input json.RawMessage) []byte {
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	data, _ := json.Marshal(controlResponseEnvelope{
		Type: "control_response",
		Response: controlResponseBody{
			RequestID: requestID,
			Subtype:   "success",
			Response:  &controlResponseResult{Behavior: "allow", UpdatedInput: input},
		},
	})
	return data
}

// buildControlResponseDeny builds the exact deny control_response bytes
// pinned in ../relay/docs/ssh-hosts.md.
func buildControlResponseDeny(requestID json.RawMessage, message string) []byte {
	data, _ := json.Marshal(controlResponseEnvelope{
		Type: "control_response",
		Response: controlResponseBody{
			RequestID: requestID,
			Subtype:   "success",
			Response:  &controlResponseResult{Behavior: "deny", Message: message},
		},
	})
	return data
}

// buildControlResponseUnsupported answers a control_request subtype this
// provider doesn't implement, so the CLI never blocks waiting on us.
func buildControlResponseUnsupported(requestID json.RawMessage) []byte {
	data, _ := json.Marshal(controlResponseEnvelope{
		Type: "control_response",
		Response: controlResponseBody{
			RequestID: requestID,
			Subtype:   "error",
			Error:     "unsupported",
		},
	})
	return data
}

// handleControlRequest dispatches an incoming control_request line. Only
// subtype "can_use_tool" is implemented; anything else gets the "unsupported"
// error response so Claude's stdio permission channel never blocks
// indefinitely on a request type this provider doesn't know about.
func (p *ClaudeProvider) handleControlRequest(raw json.RawMessage) {
	var req claudeControlRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		slog.Warn("claude: malformed control_request", "session", p.session.ID, "error", err)
		return
	}

	if req.Request.Subtype != "can_use_tool" {
		p.writeControlResponse(buildControlResponseUnsupported(req.RequestID))
		return
	}

	toolInput := string(req.Request.Input)
	if toolInput == "" {
		toolInput = "{}"
	}

	// Evaluate the session's policy exactly as /api/permission does: deny,
	// then allow, before ever bothering a viewer.
	if policy := p.session.Policy; policy != nil {
		if MatchToolRule(req.Request.ToolName, toolInput, policy.DeniedTools) {
			p.writeControlResponse(buildControlResponseDeny(req.RequestID, "denied by project policy"))
			return
		}
		if MatchToolRule(req.Request.ToolName, toolInput, policy.AllowedTools) {
			p.writeControlResponse(buildControlResponseAllow(req.RequestID, req.Request.Input))
			return
		}
	}

	if p.perms == nil {
		p.writeControlResponse(buildControlResponseDeny(req.RequestID, "no permission manager configured"))
		return
	}

	pending, ch := p.perms.CreateRequest(p.session.ID, req.Request.ToolName, toolInput, req.Request.ToolUseID)
	if p.perms.sink != nil {
		p.perms.sink.SendToSession(p.session.ID, map[string]interface{}{
			"type":         WSMsgPermissionRequest,
			"sessionId":    p.session.ID,
			"permissionId": pending.ID,
			"toolName":     req.Request.ToolName,
			"toolInput":    toolInput,
			"toolUseId":    req.Request.ToolUseID,
		})
	}

	// Resolution (a permission_response from Eve, a timeout, or a
	// stop/kill's DenyAllForSession) arrives on ch from another goroutine;
	// wait for it off the stdout-reading goroutine so a slow decision never
	// stalls reading the rest of Claude's output.
	go func() {
		decision := p.perms.WaitForDecision(pending.ID, ch)
		if decision.Decision == "allow" {
			p.writeControlResponse(buildControlResponseAllow(req.RequestID, req.Request.Input))
			return
		}
		reason := decision.Reason
		if reason == "" {
			reason = "Denied by user"
		}
		p.writeControlResponse(buildControlResponseDeny(req.RequestID, reason))
	}()
}

// writeControlResponse writes one control_response line to Claude's stdin.
// Shares p.mu with SendMessage since both write to the same pipe.
func (p *ClaudeProvider) writeControlResponse(data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin == nil {
		return
	}
	if _, err := p.stdin.Write(append(data, '\n')); err != nil {
		slog.Warn("claude: write control_response failed", "session", p.session.ID, "error", err)
	}
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
	// The process that would read a control_response is going away — deny
	// every request that's still waiting on one rather than let it burn its
	// full 60s timeout. No-op if p.perms is nil or nothing is pending.
	if p.perms != nil {
		p.perms.DenyAllForSession(p.session.ID, "session stopped")
	}

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

// SetPermissionMode kills the running Claude subprocess and respawns it with
// the new --permission-mode flag, using --resume so the conversation history
// is preserved (Claude reloads from ~/.claude/projects/.../<sid>.jsonl).
//
// Refuses to switch while the session is mid-turn — killing Claude there
// drops the in-flight assistant response that --resume can't recover.
//
// Mode is one of: default, acceptEdits, plan, bypassPermissions.
// Empty string is treated as "default".
func (p *ClaudeProvider) SetPermissionMode(mode string) error {
	switch mode {
	case "", "default", "acceptEdits", "plan", "bypassPermissions":
	default:
		return fmt.Errorf("invalid permission mode: %q", mode)
	}
	if mode == "" {
		mode = "default"
	}

	p.session.mu.Lock()
	if p.session.processing {
		p.session.mu.Unlock()
		return fmt.Errorf("cannot change permission mode while session is generating; stop the response first")
	}
	p.session.PermissionMode = mode
	p.session.Headless = (mode == "bypassPermissions")
	p.session.mu.Unlock()

	if p.Alive() {
		p.Kill()
	}
	return p.Start()
}

func (p *ClaudeProvider) DeleteSession() error {
	p.mu.Lock()
	sid := p.claudeSessionID
	p.mu.Unlock()
	if sid == "" {
		return nil
	}

	if host := p.session.getHost(); host != nil {
		return deleteClaudeHistoryOverSSH(host, p.directory, sid)
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
