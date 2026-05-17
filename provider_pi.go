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

	"github.com/google/uuid"
)

const piIdleTimeout = 15 * time.Minute
const piRPCTimeout = 10 * time.Second

// PiProvider manages a persistent pi (pi.dev coding agent) CLI process in
// `--mode rpc`. The wire format is JSONL: commands written to stdin one per
// line, responses + events read from stdout one per line. The wrapper
// translates pi's event vocabulary (message_update, tool_execution_*,
// agent_end) into the Claude-shaped stream-json envelope so Eve can use a
// single renderer regardless of which CLI agent backs the session.
type PiProvider struct {
	session *Session
	handler EventHandler

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	mu     sync.Mutex // serializes writes to stdin
	alive  atomic.Bool

	piSessionID   string // pi's own session UUID — persisted across restarts
	provider      string // e.g., "anthropic"
	modelID       string // e.g., "claude-sonnet-4-20250514"
	thinkingLevel string // off/minimal/low/medium/high/xhigh
	directory     string
	dataDir       string   // relayLLM data dir — pi session JSONLs live under {dataDir}/pi-sessions
	binaryPath    string   // configured pi binary path (empty = resolve via fallback chain)
	extraArgs     []string // appended to argv after standard flags

	// Relay-managed spawn fields (mirror the PTY pidev template). When
	// useRelayToken or autoRegenSkills is set, Start() calls
	// RelayManagedSpec.Resolve() to regenerate the project skill and fetch
	// a project-scoped token before spawning pi.
	useRelayToken   bool
	autoRegenSkills string
	skillPathTpl    string
	envPassthrough  []string

	// RPC request/response correlation. pi commands carry an optional `id`
	// field; the matching response echoes the same `id`. We register a
	// channel per outstanding request so set_model/get_state/etc. can block
	// on their specific response while async events keep flowing.
	rpcMu      sync.Mutex
	rpcPending map[string]chan json.RawMessage

	// Canonical event emitter (events.go) — produces the `type:"assistant"`
	// wire format Eve renders for Ollama/OpenAI too. Initialized in Start.
	emitter *EventEmitter

	// Per-turn translation state. Pi opens content blocks at sparse/
	// re-usable `contentIndex` values and may open block N+1 before
	// closing block N; we assign our own monotonic relay-side index and
	// auto-close the previously open block when a new *_start arrives.
	// allBlocks accumulates completed blocks for the assistant message
	// that gets appended to session.Messages on agent_end (mirrors how
	// chat_base persists state.blocks before emitting message_complete).
	streamMu        sync.Mutex
	currentBlockIdx int
	piIdxToRelay    map[int]int
	openRelayIdx    int             // 0-based relay index of the currently open block
	openKind        string          // BlockText / BlockThinking / BlockToolUse — empty when no block open
	openText        strings.Builder // accumulated text/thinking content for the open block
	openToolID      string          // tool_use only
	openToolName    string          // tool_use only
	openToolArgs    strings.Builder // tool_use input_json fragments
	allBlocks       []map[string]any
	// Tool names indexed by id, harvested from tool_execution_start /
	// tool_execution_end events. Pi's assistantMessageEvent.toolcall_start
	// doesn't reliably carry the name (some pi versions only put it on the
	// top-level tool_execution events); this is the fallback so we never
	// emit a tool_use block with an empty name.
	toolNamesByID map[string]string

	lastActivity atomic.Int64
	stopIdle     chan struct{}
	stopIdleOnce sync.Once
	waitDone     chan struct{}

	// Per-turn timing for TTFT / TPS metrics.
	msgStartNano   atomic.Int64
	firstTokenNano atomic.Int64
}

func NewPiProvider(session *Session, handler EventHandler, provider, modelID, dataDir string, cfg *PiConfig) *PiProvider {
	p := &PiProvider{
		session:       session,
		handler:       handler,
		provider:      provider,
		modelID:       modelID,
		thinkingLevel: session.ThinkingLevel,
		directory:     session.Directory,
		dataDir:       dataDir,
		rpcPending:    make(map[string]chan json.RawMessage),
		piIdxToRelay:  make(map[int]int),
		toolNamesByID: make(map[string]string),
	}
	p.emitter = NewEventEmitter(handler)
	if cfg != nil {
		p.binaryPath = cfg.BinaryPath
		p.extraArgs = cfg.ExtraArgs
		p.useRelayToken = cfg.UseRelayToken
		p.autoRegenSkills = cfg.AutoRegenSkills
		p.skillPathTpl = cfg.SkillPath
		p.envPassthrough = cfg.EnvPassthrough
	}
	return p
}

func (p *PiProvider) touchActivity() {
	p.lastActivity.Store(time.Now().Unix())
}

func (p *PiProvider) idleWatcher() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopIdle:
			return
		case <-ticker.C:
			idle := time.Now().Unix() - p.lastActivity.Load()
			if idle > int64(piIdleTimeout.Seconds()) {
				slog.Info("pi process idle, killing", "session", p.session.ID, "idleSecs", idle)
				p.Kill()
				return
			}
		}
	}
}

func (p *PiProvider) sessionDir() string {
	return filepath.Join(p.dataDir, "pi-sessions")
}

func (p *PiProvider) Start() error {
	// Relay-managed spawn prep: regenerate skill, fetch project token,
	// expose ${SKILL_PATH}/${RELAY_TOKEN}/${project.path} for extraArgs.
	// No-op when none of the relay-managed fields are set.
	subs, err := RelayManagedSpec{
		Directory:       p.directory,
		SkillPath:       p.skillPathTpl,
		AutoRegenSkills: p.autoRegenSkills,
		UseRelayToken:   p.useRelayToken,
		Label:           "pi",
	}.Resolve()
	if err != nil {
		return fmt.Errorf("pi: %w", err)
	}

	args := []string{"--mode", "rpc"}

	if p.provider != "" {
		args = append(args, "--provider", p.provider)
	}
	if p.modelID != "" {
		args = append(args, "--model", p.modelID)
	}
	if p.thinkingLevel != "" {
		args = append(args, "--thinking", p.thinkingLevel)
	}
	if p.piSessionID != "" {
		args = append(args, "--session", p.piSessionID)
	}

	sessionDir := p.sessionDir()
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return fmt.Errorf("create pi session dir: %w", err)
	}
	args = append(args, "--session-dir", sessionDir)

	if p.session.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", p.session.SystemPrompt)
	}

	// Auto-append --skill <skills-root> so pi recursively discovers every
	// sibling skill at once (e.g. .claude/skills/ containing relay/, tbo-email/,
	// import-meeting/ — one flag, all three loaded). When relay-managed, use
	// the parent of the resolved skill path; otherwise fall back to
	// <project>/.claude/skills if present. Skip when the user wired --skill
	// themselves via extraArgs (avoid duplicate flags).
	if !hasArg(p.extraArgs, "--skill") {
		skillsRoot := ""
		if subs.SkillPath != "" {
			skillsRoot = filepath.Dir(subs.SkillPath)
		} else if p.directory != "" {
			candidate := filepath.Join(p.directory, ".claude", "skills")
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				skillsRoot = candidate
			}
		}
		if skillsRoot != "" {
			args = append(args, "--skill", skillsRoot)
		}
	}

	// Expand placeholders in user extraArgs so power users can still
	// reference ${SKILL_PATH}/${RELAY_TOKEN}/${project.path} themselves.
	for _, extra := range p.extraArgs {
		args = append(args, subs.Expand(extra))
	}

	piPath := resolvePiPath(p.binaryPath)
	cmd := exec.Command(piPath, args...)
	cmd.Dir = p.directory
	cmd.Env = ensurePath(os.Environ())
	cmd.Env = append(cmd.Env,
		"PI_OFFLINE=1",
		"PI_SKIP_VERSION_CHECK=1",
	)
	if subs.RelayToken != "" {
		cmd.Env = setEnv(cmd.Env, "RELAY_TOKEN", subs.RelayToken)
	}
	cmd.Env = applyEnvPassthrough(cmd.Env, p.envPassthrough)

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
		return fmt.Errorf("failed to start pi: %w", err)
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

	slog.Info("pi process started", "session", p.session.ID, "model", p.modelID, "pid", cmd.Process.Pid)

	// Capture pi's own session UUID once the subprocess is up. Run async so
	// Start() doesn't block — pi accepts get_state immediately after spawn.
	go p.fetchInitialState()

	return nil
}

func (p *PiProvider) fetchInitialState() {
	resp, err := p.sendRPC(map[string]interface{}{"type": "get_state"})
	if err != nil {
		slog.Warn("pi get_state failed", "session", p.session.ID, "error", err)
		return
	}
	var r struct {
		Success bool `json:"success"`
		Data    struct {
			SessionID string `json:"sessionId"`
		} `json:"data"`
	}
	if json.Unmarshal(resp, &r) == nil && r.Success && r.Data.SessionID != "" {
		p.mu.Lock()
		p.piSessionID = r.Data.SessionID
		p.mu.Unlock()
	}
}

func (p *PiProvider) readStdout(r io.ReadCloser) {
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
		slog.Error("pi stdout read error", "session", p.session.ID, "error", err)
	}
}

func (p *PiProvider) readStderr(r io.ReadCloser) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		text := scanner.Text()
		if text != "" {
			slog.Debug("pi stderr", "session", p.session.ID, "text", text)
		}
	}
}

func (p *PiProvider) waitForExit() {
	err := p.cmd.Wait()
	p.alive.Store(false)
	close(p.waitDone)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}

	slog.Info("pi process exited", "session", p.session.ID, "exitCode", exitCode)

	data, _ := json.Marshal(map[string]interface{}{
		"exitCode": exitCode,
	})
	p.handler("process_exited", data)
}

// processLine routes one JSONL frame from pi. Three categories:
//   - response: RPC reply, delivered to the waiter via rpcPending.
//   - asynchronous event: translated into Claude-shape stream-json.
//   - unknown: forwarded as raw_output so it surfaces in logs.
func (p *PiProvider) processLine(raw json.RawMessage) {
	p.touchActivity()

	var envelope struct {
		Type string `json:"type"`
		ID   string `json:"id,omitempty"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		p.handler("raw_output", raw)
		return
	}

	if envelope.Type == "response" {
		p.deliverResponse(envelope.ID, raw)
		return
	}

	p.translate(envelope.Type, raw)
}

func (p *PiProvider) deliverResponse(id string, raw json.RawMessage) {
	if id == "" {
		return
	}
	p.rpcMu.Lock()
	ch, ok := p.rpcPending[id]
	if ok {
		delete(p.rpcPending, id)
	}
	p.rpcMu.Unlock()
	if ok {
		select {
		case ch <- raw:
		default:
		}
	}
}

// allocBlockIndex returns the relay-side index for a given pi contentIndex,
// allocating a new monotonic index the first time we see it within this turn.
func (p *PiProvider) allocBlockIndex(piIdx int) int {
	p.streamMu.Lock()
	defer p.streamMu.Unlock()
	if idx, ok := p.piIdxToRelay[piIdx]; ok {
		return idx
	}
	idx := p.currentBlockIdx
	p.currentBlockIdx++
	p.piIdxToRelay[piIdx] = idx
	return idx
}

// finalizeOpenBlock emits BlockStop / ToolUseBlockStop for whatever block
// is currently open and pushes its accumulated content onto allBlocks.
// No-op if no block is open. MUST be called with streamMu held.
func (p *PiProvider) finalizeOpenBlockLocked() {
	switch p.openKind {
	case BlockText:
		p.emitter.BlockStop(p.openRelayIdx)
		p.allBlocks = append(p.allBlocks, map[string]any{
			"type": BlockText,
			"text": p.openText.String(),
		})
	case BlockThinking:
		p.emitter.BlockStop(p.openRelayIdx)
		p.allBlocks = append(p.allBlocks, map[string]any{
			"type":     BlockThinking,
			"thinking": p.openText.String(),
		})
	case BlockToolUse:
		var input json.RawMessage
		if s := p.openToolArgs.String(); s != "" {
			input = json.RawMessage(s)
		}
		p.emitter.ToolUseBlockStop(p.openRelayIdx, p.openToolID, p.openToolName, input)
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		p.allBlocks = append(p.allBlocks, map[string]any{
			"type":  BlockToolUse,
			"id":    p.openToolID,
			"name":  p.openToolName,
			"input": input,
		})
	}
	p.openKind = ""
	p.openText.Reset()
	p.openToolID = ""
	p.openToolName = ""
	p.openToolArgs.Reset()
}

// startBlock closes whatever block is currently open (if any) and marks
// idx/kind as the new active block.
func (p *PiProvider) startBlock(idx int, kind string) {
	p.streamMu.Lock()
	defer p.streamMu.Unlock()
	if p.openKind != "" && p.openRelayIdx != idx {
		p.finalizeOpenBlockLocked()
	}
	p.openRelayIdx = idx
	p.openKind = kind
}

// endBlock closes idx if it's the currently open block. Returns false if
// the block was already closed (pi sometimes emits out-of-order _end after
// auto-close).
func (p *PiProvider) endBlock(idx int) bool {
	p.streamMu.Lock()
	defer p.streamMu.Unlock()
	if p.openKind == "" || p.openRelayIdx != idx {
		return false
	}
	p.finalizeOpenBlockLocked()
	return true
}

func (p *PiProvider) appendText(s string)     { p.streamMu.Lock(); p.openText.WriteString(s); p.streamMu.Unlock() }
func (p *PiProvider) appendToolArgs(s string) { p.streamMu.Lock(); p.openToolArgs.WriteString(s); p.streamMu.Unlock() }
func (p *PiProvider) setOpenTool(id, name string) {
	p.streamMu.Lock()
	p.openToolID = id
	p.openToolName = name
	p.streamMu.Unlock()
}

func (p *PiProvider) resetTurnState() {
	p.streamMu.Lock()
	p.currentBlockIdx = 0
	p.piIdxToRelay = make(map[int]int)
	p.openKind = ""
	p.openRelayIdx = 0
	p.openText.Reset()
	p.openToolID = ""
	p.openToolName = ""
	p.openToolArgs.Reset()
	p.allBlocks = nil
	p.toolNamesByID = make(map[string]string)
	p.streamMu.Unlock()
}

// rememberToolName caches (id → name) harvested from tool_execution_* events
// so resolveToolIdentity can fall back when toolcall_start's nested payload
// doesn't carry a name.
func (p *PiProvider) rememberToolName(id, name string) {
	if id == "" || name == "" {
		return
	}
	p.streamMu.Lock()
	p.toolNamesByID[id] = name
	p.streamMu.Unlock()
}

func (p *PiProvider) lookupToolName(id string) string {
	if id == "" {
		return ""
	}
	p.streamMu.Lock()
	name := p.toolNamesByID[id]
	p.streamMu.Unlock()
	return name
}

// flushOpenBlock closes any open block at turn end (defensive — pi should
// always emit a final _end, but this guarantees we never leave a block
// dangling).
func (p *PiProvider) flushOpenBlock() {
	p.streamMu.Lock()
	defer p.streamMu.Unlock()
	if p.openKind != "" {
		p.finalizeOpenBlockLocked()
	}
}

// translate converts a pi RPC event to one or more canonical relay
// llm_event frames (events.go format) and emits them via the EventEmitter.
func (p *PiProvider) translate(eventType string, raw json.RawMessage) {
	switch eventType {
	case "agent_start":
		p.resetTurnState()
		p.firstTokenNano.Store(0)
		p.mu.Lock()
		modelID := p.modelID
		p.mu.Unlock()
		p.emitter.SystemInit(modelID, p.directory, nil, nil)
		p.emitter.MessageStart("")

	case "message_update":
		p.translateMessageUpdate(raw)

	case "tool_execution_end":
		p.translateToolResult(raw)

	case "agent_end":
		p.translateAgentEnd(raw)

	case "tool_execution_start", "tool_execution_update":
		// Pi emits the canonical {toolCallId, toolName} pair on these
		// top-level events. Harvest them so toolcall_start (whose nested
		// toolCall payload is unreliable across pi versions) can fall back
		// to a real name instead of leaving the wire field empty.
		var ev struct {
			ToolCallID string `json:"toolCallId"`
			ToolName   string `json:"toolName"`
		}
		if json.Unmarshal(raw, &ev) == nil {
			p.rememberToolName(ev.ToolCallID, ev.ToolName)
		}

	case "message_start", "message_end", "turn_start", "turn_end":
		// Bookkeeping events — already covered by the finer message_update
		// translations. Skip silently.

	case "auto_retry_start", "auto_retry_end":
		p.emitRetryNotice(eventType, raw)

	default:
		// queue_update, compaction_*, extension_error, extension_ui_request —
		// log via raw_output so operators can see them without crashing the
		// renderer.
		p.handler("raw_output", raw)
	}
}

// emitRetryNotice turns pi's auto_retry_{start,end} payloads into a short
// one-line message routed through raw_output. The original JSON dump is too
// noisy for chat UI; this preserves visibility of "something is being
// retried" without showing the raw envelope.
func (p *PiProvider) emitRetryNotice(eventType string, raw json.RawMessage) {
	var text string
	switch eventType {
	case "auto_retry_start":
		var ev struct {
			Attempt      int     `json:"attempt"`
			MaxAttempts  int     `json:"maxAttempts"`
			DelayMs      float64 `json:"delayMs"`
			ErrorMessage string  `json:"errorMessage"`
		}
		_ = json.Unmarshal(raw, &ev)
		text = fmt.Sprintf("Retry %d/%d in %.0fs — %s",
			ev.Attempt, ev.MaxAttempts, ev.DelayMs/1000, shortenError(ev.ErrorMessage))
	case "auto_retry_end":
		var ev struct {
			Success    bool   `json:"success"`
			Attempt    int    `json:"attempt"`
			FinalError string `json:"finalError"`
		}
		_ = json.Unmarshal(raw, &ev)
		if ev.Success {
			text = fmt.Sprintf("Retry succeeded on attempt %d", ev.Attempt)
		} else {
			text = fmt.Sprintf("Retry failed after %d attempts — %s",
				ev.Attempt, shortenError(ev.FinalError))
		}
	}
	p.handler("raw_output", json.RawMessage([]byte(text)))
}

// shortenError trims provider-prefixed status codes and overly long
// payloads from a transient-error string so the chat notice stays one line.
func shortenError(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown error"
	}
	// Cut at first '{' to drop embedded JSON bodies pi includes verbatim.
	if i := strings.Index(s, "{"); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	const max = 120
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// piContentBlock describes the relevant subset of an assistantMessageEvent
// delta. Pi has shipped two schemas for tool calls: older versions emit a
// nested toolCall:{id, name} object on toolcall_start; newer versions emit
// flat toolCallId/toolName fields at the assistantMessageEvent level (the
// same shape its tool_execution_* events use). resolveToolIdentity reads
// whichever is populated, with a final fallback to the harvested map.
type piContentBlock struct {
	Type         string          `json:"type"`
	ContentIndex int             `json:"contentIndex"`
	Delta        string          `json:"delta,omitempty"`
	Content      string          `json:"content,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	ToolCall     *piToolCall     `json:"toolCall,omitempty"`
	ToolCallID   string          `json:"toolCallId,omitempty"`
	ToolName     string          `json:"toolName,omitempty"`
	Partial      json.RawMessage `json:"partial,omitempty"`
}

type piToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// resolveToolIdentity returns the best (id, name) pair available for a pi
// tool_use block. It checks the nested toolCall object, then the flat
// top-level fields, then the harvested-from-tool_execution_start cache. If
// the name is still empty we log and fall back to a synthetic placeholder
// rather than ship "undefined" to the renderer — pi will surface the real
// name on the subsequent tool_execution_end and the result block still
// pairs correctly by id.
func (p *PiProvider) resolveToolIdentity(ev piContentBlock) (id, name string) {
	if ev.ToolCall != nil {
		id, name = ev.ToolCall.ID, ev.ToolCall.Name
	}
	if id == "" {
		id = ev.ToolCallID
	}
	if name == "" {
		name = ev.ToolName
	}
	if name == "" {
		name = p.lookupToolName(id)
	}
	if name == "" {
		slog.Warn("pi: toolcall_start missing tool name", "session", p.session.ID, "toolCallId", id)
		name = piMissingToolNamePlaceholder
	}
	return id, name
}

// piMissingToolNamePlaceholder is the wire-level fallback when none of the
// pi name channels (nested toolCall, flat fields, harvested cache) yield a
// value. Surfaces as a generic label in the UI while the slog.Warn captures
// the toolCallId for investigation.
const piMissingToolNamePlaceholder = "tool"

func (p *PiProvider) translateMessageUpdate(raw json.RawMessage) {
	var msg struct {
		AssistantMessageEvent piContentBlock `json:"assistantMessageEvent"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}
	ev := msg.AssistantMessageEvent
	switch ev.Type {
	case "start":
		// Pi's `start` is bookkeeping; agent_start already opened the turn.

	case "text_start":
		p.firstTokenNano.CompareAndSwap(0, time.Now().UnixNano())
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.startBlock(idx, BlockText)
		p.emitter.TextBlockStart(idx)

	case "text_delta":
		p.firstTokenNano.CompareAndSwap(0, time.Now().UnixNano())
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.emitter.TextDelta(idx, ev.Delta)
		p.appendText(ev.Delta)

	case "text_end":
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.endBlock(idx)

	case "thinking_start":
		p.firstTokenNano.CompareAndSwap(0, time.Now().UnixNano())
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.startBlock(idx, BlockThinking)
		p.emitter.ThinkingBlockStart(idx)

	case "thinking_delta":
		p.firstTokenNano.CompareAndSwap(0, time.Now().UnixNano())
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.emitter.ThinkingDelta(idx, ev.Delta)
		p.appendText(ev.Delta)

	case "thinking_end":
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.endBlock(idx)

	case "toolcall_start":
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.startBlock(idx, BlockToolUse)
		id, name := p.resolveToolIdentity(ev)
		p.setOpenTool(id, name)
		p.emitter.ToolUseBlockStart(idx, id, name)

	case "toolcall_delta":
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.emitter.InputJsonDelta(idx, ev.Delta)
		p.appendToolArgs(ev.Delta)

	case "toolcall_end":
		idx := p.allocBlockIndex(ev.ContentIndex)
		p.endBlock(idx)

	case "done":
		// Pi's `done` carries the model's stop reason. The canonical relay
		// protocol doesn't have a dedicated stop_reason event; chat_base
		// signals turn end via stats_update + message_complete. We do the
		// same in translateAgentEnd, so `done` is informational here.

	case "error":
		p.emitter.ResultError(ev.Reason)
	}
}

func (p *PiProvider) translateToolResult(raw json.RawMessage) {
	var ev struct {
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
		Result     struct {
			Content json.RawMessage `json:"content"`
		} `json:"result"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return
	}
	p.emitter.ToolResult(ev.ToolCallID, ev.ToolName, flattenTextBlocks(ev.Result.Content), ev.IsError)
}

func (p *PiProvider) translateAgentEnd(raw json.RawMessage) {
	// Defensive: close any block pi forgot to close (shouldn't happen, but
	// guarantees state.blocks is complete).
	p.flushOpenBlock()

	// Pi's agent_end carries the assistant messages, each with its own
	// usage block. Extract from there — we can't call get_session_stats
	// over RPC because we ARE the readStdout goroutine that delivers
	// responses, so a sync RPC here would deadlock until the 10s timeout.
	stats := extractUsageFromAgentEnd(raw)

	// Compute TTFT/TPS from the per-turn timestamps.
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

	// Persist the assistant turn as canonical content blocks (mirrors
	// chat_base lines 402-407). Pi owns its own per-session JSONL too,
	// but Eve's history view reads from session.Messages, so we MUST
	// persist here for the UI to show the turn after a reload.
	p.streamMu.Lock()
	blocks := p.allBlocks
	p.allBlocks = nil
	p.streamMu.Unlock()
	if len(blocks) > 0 {
		contentJSON, _ := json.Marshal(blocks)
		p.session.mu.Lock()
		p.session.Messages = append(p.session.Messages, Message{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Role:      "assistant",
			Content:   contentJSON,
		})
		p.session.mu.Unlock()
	}

	// nil data → session layer skips its fallback text-only save (we just
	// did the canonical save above).
	p.handler("message_complete", nil)
}

// extractUsageFromAgentEnd sums per-message usage from an agent_end event.
// Pi reports per-assistant-message tokens, so summing covers multi-turn
// agent runs (e.g. several tool-use rounds within one prompt).
func extractUsageFromAgentEnd(raw json.RawMessage) SessionStats {
	var ev struct {
		Messages []struct {
			Role  string `json:"role"`
			Usage struct {
				Input      int     `json:"input"`
				Output     int     `json:"output"`
				CacheRead  int     `json:"cacheRead"`
				CacheWrite int     `json:"cacheWrite"`
				Cost       struct {
					Total float64 `json:"total"`
				} `json:"cost"`
			} `json:"usage"`
		} `json:"messages"`
	}
	var s SessionStats
	if err := json.Unmarshal(raw, &ev); err != nil {
		return s
	}
	for _, m := range ev.Messages {
		if m.Role != "assistant" {
			continue
		}
		s.InputTokens += m.Usage.Input
		s.OutputTokens += m.Usage.Output
		s.CacheReadTokens += m.Usage.CacheRead
		s.CacheCreationTokens += m.Usage.CacheWrite
		s.CostUsd += m.Usage.Cost.Total
	}
	return s
}

// sendRPC writes a command to pi's stdin and blocks until the matching
// response arrives or piRPCTimeout elapses. Returns the raw response line.
func (p *PiProvider) sendRPC(cmd map[string]interface{}) (json.RawMessage, error) {
	id := uuid.New().String()
	cmd["id"] = id

	ch := make(chan json.RawMessage, 1)
	p.rpcMu.Lock()
	p.rpcPending[id] = ch
	p.rpcMu.Unlock()

	cleanup := func() {
		p.rpcMu.Lock()
		delete(p.rpcPending, id)
		p.rpcMu.Unlock()
	}

	data, err := json.Marshal(cmd)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("marshal rpc: %w", err)
	}
	data = append(data, '\n')

	p.mu.Lock()
	if !p.alive.Load() || p.stdin == nil {
		p.mu.Unlock()
		cleanup()
		return nil, fmt.Errorf("pi process not running")
	}
	_, err = p.stdin.Write(data)
	p.mu.Unlock()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("write rpc: %w", err)
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("rpc aborted: %s (process killed)", cmd["type"])
		}
		return resp, nil
	case <-time.After(piRPCTimeout):
		cleanup()
		return nil, fmt.Errorf("rpc timeout: %s", cmd["type"])
	}
}

func (p *PiProvider) SendMessage(text string, files []FileAttachment) error {
	if !p.alive.Load() || p.stdin == nil {
		return fmt.Errorf("pi process not running")
	}

	p.touchActivity()

	cmd := map[string]interface{}{
		"id":      uuid.New().String(),
		"type":    "prompt",
		"message": text,
	}
	if len(files) > 0 {
		images := make([]map[string]interface{}, 0, len(files))
		for _, f := range files {
			images = append(images, map[string]interface{}{
				"type":     "image",
				"data":     f.Data,
				"mimeType": f.MimeType,
			})
		}
		cmd["images"] = images
	}

	data, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal prompt: %w", err)
	}
	data = append(data, '\n')

	// Record turn start before writing so first-token latency is measured
	// from when pi gets the command, not when the response channel opens.
	p.msgStartNano.Store(time.Now().UnixNano())
	p.firstTokenNano.Store(0)

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.stdin.Write(data); err != nil {
		return fmt.Errorf("write to stdin: %w", err)
	}
	return nil
}

func (p *PiProvider) StopGeneration() {
	if !p.alive.Load() || p.stdin == nil {
		return
	}
	// pi's abort is an in-band cancel — the subprocess stays alive and we
	// can keep sending prompts on it. Fall back to Kill on write failure.
	cmd := map[string]interface{}{"type": "abort"}
	data, _ := json.Marshal(cmd)
	data = append(data, '\n')

	p.mu.Lock()
	_, err := p.stdin.Write(data)
	p.mu.Unlock()
	if err != nil {
		slog.Warn("pi abort write failed, killing", "session", p.session.ID, "error", err)
		p.Kill()
	}
}

func (p *PiProvider) Kill() {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}

	// Mark dead early so concurrent SendMessage calls fail fast instead of
	// writing to a closing stdin pipe.
	p.alive.Store(false)

	if p.stopIdle != nil {
		p.stopIdleOnce.Do(func() { close(p.stopIdle) })
	}

	// Unblock any in-flight RPC waiters — their responses will never
	// arrive once stdin closes, and sendRPC would otherwise hold its
	// channel until the 10s timeout, blocking SetModel/SetThinkingLevel
	// callers (and leaking goroutines if they retry).
	p.rpcMu.Lock()
	for id, ch := range p.rpcPending {
		close(ch)
		delete(p.rpcPending, id)
	}
	p.rpcMu.Unlock()

	if p.stdin != nil {
		p.stdin.Close()
	}

	_ = p.cmd.Process.Signal(os.Interrupt)

	select {
	case <-p.waitDone:
	case <-time.After(3 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.waitDone
	}

	slog.Info("pi process killed", "session", p.session.ID)
}

func (p *PiProvider) Alive() bool {
	return p.alive.Load()
}

// SetModel switches the active provider/model mid-session via pi's
// set_model RPC. Refuses while the session is mid-turn — pi accepts
// set_model only between turns and a partial assistant response would be
// lost.
func (p *PiProvider) SetModel(provider, modelID string) error {
	if provider == "" || modelID == "" {
		return fmt.Errorf("provider and modelId are required")
	}

	p.session.mu.Lock()
	if p.session.processing {
		p.session.mu.Unlock()
		return fmt.Errorf("cannot change model while session is generating; stop the response first")
	}
	p.session.mu.Unlock()

	resp, err := p.sendRPC(map[string]interface{}{
		"type":     "set_model",
		"provider": provider,
		"modelId":  modelID,
	})
	if err != nil {
		return err
	}
	var r struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(resp, &r); err != nil {
		return fmt.Errorf("parse set_model response: %w", err)
	}
	if !r.Success {
		return fmt.Errorf("set_model rejected: %s", r.Error)
	}

	p.mu.Lock()
	p.provider = provider
	p.modelID = modelID
	p.mu.Unlock()

	p.session.mu.Lock()
	p.session.Model = piModelString(provider, modelID)
	p.session.mu.Unlock()
	return nil
}

// SetThinkingLevel changes pi's thinking/reasoning level via the
// set_thinking_level RPC. Valid levels: off, minimal, low, medium, high,
// xhigh (xhigh is OpenAI-codex-max only).
func (p *PiProvider) SetThinkingLevel(level string) error {
	switch level {
	case "off", "minimal", "low", "medium", "high", "xhigh":
	default:
		return fmt.Errorf("invalid thinking level: %q", level)
	}

	resp, err := p.sendRPC(map[string]interface{}{
		"type":  "set_thinking_level",
		"level": level,
	})
	if err != nil {
		return err
	}
	var r struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(resp, &r); err != nil {
		return fmt.Errorf("parse set_thinking_level response: %w", err)
	}
	if !r.Success {
		return fmt.Errorf("set_thinking_level rejected: %s", r.Error)
	}

	p.mu.Lock()
	p.thinkingLevel = level
	p.mu.Unlock()

	p.session.mu.Lock()
	p.session.ThinkingLevel = level
	p.session.mu.Unlock()
	return nil
}

func (p *PiProvider) DeleteSession() error {
	p.mu.Lock()
	sid := p.piSessionID
	p.mu.Unlock()
	if sid == "" {
		return nil
	}

	// Pi paths look like: {sessionDir}/--<cwd-with-slashes-as-dashes>--/<timestamp>_<uuid>.jsonl
	pattern := filepath.Join(p.sessionDir(), "*", "*_"+sid+".jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob pi session: %w", err)
	}
	for _, path := range matches {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove pi session file: %w", err)
		}
		slog.Info("deleted pi session file", "path", path)
	}
	return nil
}

func (p *PiProvider) GetState() json.RawMessage {
	p.mu.Lock()
	sid := p.piSessionID
	p.mu.Unlock()
	data, _ := json.Marshal(map[string]interface{}{
		"piSessionId": sid,
	})
	return data
}

func (p *PiProvider) RestoreState(state json.RawMessage) {
	if state == nil {
		return
	}
	var s struct {
		PiSessionID string `json:"piSessionId"`
	}
	if err := json.Unmarshal(state, &s); err == nil {
		p.mu.Lock()
		p.piSessionID = s.PiSessionID
		p.mu.Unlock()
	}
}

// resolvePiPath finds the pi binary. Priority:
//
//  1. configured path (from PiConfig.BinaryPath), expanded for ~
//  2. well-known install locations (~/.local/bin, npm globals, brew, /usr/local)
//  3. PATH lookup
//  4. literal "pi" — exec.LookPath defers the error to spawn time
//
// Pi is npm-installed so it usually lives in the user's npm global bin or
// the .local/bin path.
func resolvePiPath(configured string) string {
	if configured != "" {
		if strings.HasPrefix(configured, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				configured = filepath.Join(home, configured[2:])
			}
		}
		return configured
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".bun", "bin", "pi"),
		filepath.Join(home, ".local", "bin", "pi"),
		filepath.Join(home, ".npm-global", "bin", "pi"),
		filepath.Join(home, ".nvm", "versions", "node", "current", "bin", "pi"),
		"/opt/homebrew/bin/pi",
		"/usr/local/bin/pi",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	if p, err := exec.LookPath("pi"); err == nil {
		return p
	}
	return "pi"
}

// piModelString reconstructs a relayLLM model identifier from the
// provider/modelID pair carried by a PiProvider.
func piModelString(provider, modelID string) string {
	return strings.Join([]string{"pi", provider, modelID}, "/")
}

// hasArg reports whether args contains a flag matching name (case-insensitive).
// Used so we don't auto-append --skill if the user already put it in extraArgs.
func hasArg(args []string, name string) bool {
	for _, a := range args {
		if strings.EqualFold(a, name) {
			return true
		}
	}
	return false
}
