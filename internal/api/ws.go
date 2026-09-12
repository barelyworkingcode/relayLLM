package api

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	clk "relayllm/internal/clock"
	"relayllm/internal/events"
	"relayllm/internal/permission"
	"relayllm/internal/provider"
	"relayllm/internal/session"
	"relayllm/internal/terminal"
	"relayllm/internal/types"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// WSHub manages WebSocket connections and routes events to them.
type WSHub struct {
	mu        sync.RWMutex
	conns     map[string]map[*wsConn]bool // sessionID → set of viewer connections
	termConns map[string]map[*wsConn]bool // terminalID → set of viewer connections
	allConns  map[*wsConn]bool            // all connected clients (for broadcast)
	sessions  *session.SessionManager
	perms     *permission.PermissionManager
	terminals *terminal.TerminalManager

	// clock times each connection's activity for GET /api/status/detailed.
	// Defaults to DefaultClock; SetClock is a setter (mirroring
	// PermissionManager.SetClock) rather than a constructor parameter, so the
	// three existing NewWSHub call sites stay unchanged.
	clock clk.Clock
}

// wsConn wraps one live WebSocket connection. id/remoteAddr/connectedAt are
// immutable after newWSConn; the counters are atomics because a status poll
// (SnapshotConnections) reads them from a goroutine that holds neither wc.mu
// nor the hub lock.
type wsConn struct {
	conn *websocket.Conn
	mu   sync.Mutex

	id          uint64
	remoteAddr  string
	connectedAt time.Time
	clock       clk.Clock

	lastActivityNano atomic.Int64
	bytesOut         atomic.Int64
	bytesIn          atomic.Int64
	msgsOut          atomic.Int64
	msgsIn           atomic.Int64
}

var wsConnSeq atomic.Uint64

// newWSConn stamps a fresh sequence id and starts the activity clock at
// connect time, so a connection that has neither sent nor received anything
// yet still reports a sane (zero) idle duration rather than one measured from
// the Unix epoch.
func newWSConn(conn *websocket.Conn, remoteAddr string, clock clk.Clock) *wsConn {
	if clock == nil {
		clock = clk.DefaultClock
	}
	now := clock.Now()
	wc := &wsConn{
		conn:        conn,
		id:          wsConnSeq.Add(1),
		remoteAddr:  remoteAddr,
		connectedAt: now,
		clock:       clock,
	}
	wc.lastActivityNano.Store(now.UnixNano())
	return wc
}

// write is the single funnel every outbound WS message goes through —
// SendToTerminal, SendToSession, Broadcast, and sendJSON (and so
// sendWSError) all used to take wc.mu and call wc.conn.WriteMessage directly;
// routing them all through here is what lets GET /api/status/detailed report
// real bytesOut/messagesOut/lastActivity instead of guessing. Counters are
// updated outside the write lock on purpose — they are atomics, and holding
// wc.mu for them would serialize a status poll against a live terminal
// stream for no benefit.
func (wc *wsConn) write(data []byte) error {
	wc.mu.Lock()
	err := wc.conn.WriteMessage(websocket.TextMessage, data)
	wc.mu.Unlock()
	if err == nil {
		wc.bytesOut.Add(int64(len(data)))
		wc.msgsOut.Add(1)
		wc.lastActivityNano.Store(wc.clock.Now().UnixNano())
	}
	return err
}

// noteRead records one inbound WS message — called from HandleUpgrade's read
// loop right after a successful conn.ReadMessage().
func (wc *wsConn) noteRead(n int) {
	wc.bytesIn.Add(int64(n))
	wc.msgsIn.Add(1)
	wc.lastActivityNano.Store(wc.clock.Now().UnixNano())
}

// wsIdleAfter is the only threshold a WebSocket connection's state uses. A
// viewer sitting with no session/terminal traffic is the normal steady
// state, not a fault — unlike a proxy connection, there is no "stalled"
// state here and no threshold-driven alerting: nothing about an idle viewer
// socket signals a bug. Package-level var (not const) so a test can lower it.
var wsIdleAfter = 60 * time.Second

// WSConnInfo is one live WebSocket connection, as reported by
// SnapshotConnections for GET /api/status/detailed's connections[] array
// (kind "ws"). State is only ever "active" or "idle" — see wsIdleAfter's
// comment for why "stalled" doesn't apply here.
type WSConnInfo struct {
	ID             uint64   `json:"id"`
	RemoteAddr     string   `json:"remoteAddr"`
	ConnectedAt    string   `json:"connectedAt"` // RFC3339
	LastActivityAt string   `json:"lastActivityAt"`
	AgeSeconds     int      `json:"ageSeconds"`
	IdleSeconds    int      `json:"idleSeconds"` // since last read or write
	State          string   `json:"state"`
	BytesIn        int64    `json:"bytesIn"`
	BytesOut       int64    `json:"bytesOut"`
	MessagesIn     int64    `json:"messagesIn"`
	MessagesOut    int64    `json:"messagesOut"`
	Sessions       []string `json:"sessions"`  // sorted, never nil
	Terminals      []string `json:"terminals"` // sorted, never nil
}

// snapshot builds this connection's WSConnInfo row. sessions/terminals are
// supplied by the caller (SnapshotConnections), which already built the
// reverse index under the hub lock.
func (wc *wsConn) snapshot(sessions, terminals []string) WSConnInfo {
	now := wc.clock.Now()
	last := time.Unix(0, wc.lastActivityNano.Load())
	idle := now.Sub(last)
	state := "active"
	if idle > wsIdleAfter {
		state = "idle"
	}
	return WSConnInfo{
		ID:             wc.id,
		RemoteAddr:     wc.remoteAddr,
		ConnectedAt:    wc.connectedAt.UTC().Format(time.RFC3339),
		LastActivityAt: last.UTC().Format(time.RFC3339),
		AgeSeconds:     int(now.Sub(wc.connectedAt).Seconds()),
		IdleSeconds:    int(idle.Seconds()),
		State:          state,
		BytesIn:        wc.bytesIn.Load(),
		BytesOut:       wc.bytesOut.Load(),
		MessagesIn:     wc.msgsIn.Load(),
		MessagesOut:    wc.msgsOut.Load(),
		Sessions:       sessions,
		Terminals:      terminals,
	}
}

func NewWSHub(sessions *session.SessionManager, perms *permission.PermissionManager, terminals *terminal.TerminalManager) *WSHub {
	return &WSHub{
		conns:     make(map[string]map[*wsConn]bool),
		termConns: make(map[string]map[*wsConn]bool),
		allConns:  make(map[*wsConn]bool),
		sessions:  sessions,
		perms:     perms,
		terminals: terminals,
		clock:     clk.DefaultClock,
	}
}

// SetClock installs the clock used for connection activity timestamps.
// Mirrors PermissionManager.SetClock — a setter rather than a constructor
// parameter so the three existing NewWSHub call sites stay unchanged.
func (h *WSHub) SetClock(c clk.Clock) {
	if c == nil {
		c = clk.DefaultClock
	}
	h.clock = c
}

// SnapshotConnections returns one row per live WebSocket connection, with the
// sessions and terminals it is bound to. The hub's maps are keyed the other
// way (id → viewers), so the reverse index is built in the same RLock pass
// rather than being maintained on every join/leave — this runs once per
// dashboard poll over a handful of connections, and a second index would
// have to be kept correct in six more places. Only h.mu.RLock is taken, never
// wc.mu (snapshot reads atomics), so this cannot block behind a slow
// WebSocket write.
func (h *WSHub) SnapshotConnections() []WSConnInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()

	sessionsByConn := make(map[*wsConn][]string)
	for sid, viewers := range h.conns {
		for wc := range viewers {
			sessionsByConn[wc] = append(sessionsByConn[wc], sid)
		}
	}
	terminalsByConn := make(map[*wsConn][]string)
	for tid, viewers := range h.termConns {
		for wc := range viewers {
			terminalsByConn[wc] = append(terminalsByConn[wc], tid)
		}
	}

	out := make([]WSConnInfo, 0, len(h.allConns))
	for wc := range h.allConns {
		sessions := sessionsByConn[wc]
		sort.Strings(sessions)
		if sessions == nil {
			sessions = []string{}
		}
		terminals := terminalsByConn[wc]
		sort.Strings(terminals)
		if terminals == nil {
			terminals = []string{}
		}
		out = append(out, wc.snapshot(sessions, terminals))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SendToTerminal sends a message to all WebSocket clients viewing a terminal.
func (h *WSHub) SendToTerminal(terminalID string, msg map[string]interface{}) {
	h.mu.RLock()
	viewers, ok := h.termConns[terminalID]
	if !ok || len(viewers) == 0 {
		h.mu.RUnlock()
		return
	}
	// Copy the viewer set under lock to avoid holding RLock during writes.
	conns := make([]*wsConn, 0, len(viewers))
	for wc := range viewers {
		conns = append(conns, wc)
	}
	h.mu.RUnlock()

	data, err := json.Marshal(msg)
	if err != nil {
		slog.Error("failed to marshal terminal WS message", "error", err)
		return
	}

	for _, wc := range conns {
		wc.write(data)
	}
}

// SendToSession implements EventSink — broadcasts to all connections viewing this session.
func (h *WSHub) SendToSession(sessionID string, msg map[string]interface{}) {
	h.mu.RLock()
	viewers, ok := h.conns[sessionID]
	if !ok || len(viewers) == 0 {
		h.mu.RUnlock()
		return
	}
	conns := make([]*wsConn, 0, len(viewers))
	for wc := range viewers {
		conns = append(conns, wc)
	}
	h.mu.RUnlock()

	data, err := json.Marshal(msg)
	if err != nil {
		slog.Error("failed to marshal WS message", "error", err)
		return
	}

	for _, wc := range conns {
		wc.write(data)
	}
}

// HandleUpgrade handles WebSocket upgrade requests and dispatches messages.
func (h *WSHub) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("websocket upgrade failed", "error", err)
		return
	}

	slog.Info("websocket connected", "remote", r.RemoteAddr)

	wc := newWSConn(conn, r.RemoteAddr, h.clock)
	boundSessions := make(map[string]bool)
	boundTerminals := make(map[string]bool)

	h.mu.Lock()
	h.allConns[wc] = true
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.allConns, wc)
		for sid := range boundSessions {
			removeViewer(h.conns, sid, wc)
		}
		// Track terminals that lost their last viewer for idle timeout.
		var orphaned []string
		for tid := range boundTerminals {
			if removeViewer(h.termConns, tid, wc) == 0 {
				orphaned = append(orphaned, tid)
			}
		}
		h.mu.Unlock()

		// Start idle timers for terminals with no remaining viewers.
		for _, tid := range orphaned {
			h.terminals.NotifyViewerChange(tid, 0)
		}

		conn.Close()
		slog.Info("websocket disconnected", "remote", r.RemoteAddr)
	}()

	for {
		_, msgBytes, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				slog.Error("websocket read error", "error", err)
			}
			return
		}
		wc.noteRead(len(msgBytes))

		var msg struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(msgBytes, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case events.WSMsgJoinSession:
			h.handleJoinSession(wc, msgBytes, boundSessions)
		case events.WSMsgSendMessage:
			h.handleSendMessage(wc, msgBytes)
		case events.WSMsgEndSession:
			h.handleEndSession(wc, msgBytes)
		case events.WSMsgRenameSession:
			h.handleRenameSession(wc, msgBytes)
		case events.WSMsgSetSessionFolder:
			h.handleSetSessionFolder(wc, msgBytes)
		case events.WSMsgDeleteSession:
			h.handleDeleteSession(wc, msgBytes, boundSessions)
		case events.WSMsgLeaveSession:
			h.handleLeaveSession(wc, msgBytes, boundSessions)
		case events.WSMsgStopGeneration:
			h.handleStopGeneration(wc, msgBytes)
		case events.WSMsgClearSession:
			h.handleClearSession(wc, msgBytes)
		case events.WSMsgPermissionResponse:
			h.handlePermissionResponse(msgBytes)
		case events.WSMsgSetPermissionMode:
			h.handleSetPermissionMode(wc, msgBytes)
		case events.WSMsgTerminalCreate:
			h.handleTerminalCreate(wc, msgBytes, boundTerminals)
		case events.WSMsgJoinTerminal:
			h.handleJoinTerminal(wc, msgBytes, boundTerminals)
		case events.WSMsgLeaveTerminal:
			h.handleLeaveTerminal(wc, msgBytes, boundTerminals)
		case events.WSMsgTerminalInput:
			h.handleTerminalInput(wc, msgBytes)
		case events.WSMsgTerminalResize:
			h.handleTerminalResize(wc, msgBytes)
		case events.WSMsgTerminalClose:
			h.handleTerminalClose(msgBytes, boundTerminals)
		case events.WSMsgTerminalList:
			h.handleTerminalList(wc)
		case events.WSMsgTerminalReconnect:
			h.handleTerminalReconnect(wc, msgBytes, boundTerminals)
		case events.WSMsgTerminalTemplates:
			h.handleTerminalTemplates(wc)
		}
	}
}

// --- Session message handlers ---

func (h *WSHub) handleJoinSession(wc *wsConn, msgBytes []byte, boundSessions map[string]bool) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(msgBytes, &req)
	if req.SessionID == "" {
		return
	}

	session, ok := h.sessions.GetSession(req.SessionID)
	if !ok {
		sendWSError(wc, "session not found: "+req.SessionID)
		return
	}

	boundSessions[req.SessionID] = true
	h.mu.Lock()
	addViewer(h.conns, req.SessionID, wc)
	h.mu.Unlock()

	// Try reading from Claude CLI's JSONL session file first (has both user + assistant).
	var history []types.Message
	var claudeSessionID string
	if session.ProviderState != nil {
		var ps struct {
			ClaudeSessionID string `json:"claudeSessionId"`
		}
		json.Unmarshal(session.ProviderState, &ps)
		claudeSessionID = ps.ClaudeSessionID
	}
	if claudeSessionID != "" {
		if h, err := provider.ReadClaudeHistory(session.Directory, session.GetHost(), claudeSessionID); err == nil && len(h) > 0 {
			history = h
		} else if err != nil {
			slog.Debug("claude history unavailable, using session messages", "session", req.SessionID, "error", err)
		}
	}
	// Fall back to session.Messages if Claude history unavailable.
	if history == nil {
		session.Lock()
		history = make([]types.Message, len(session.Messages))
		copy(history, session.Messages)
		session.Unlock()
	}
	session.Lock()
	stats := session.Stats
	session.Unlock()

	sendJSON(wc, map[string]interface{}{
		"type":            events.WSMsgSessionJoined,
		"sessionId":       session.ID,
		"projectId":       session.ProjectID,
		"directory":       session.Directory,
		"model":           session.Model,
		"name":            session.Name,
		"folder":          session.Folder,
		"history":         history,
		"stats":           stats,
		"headless":        session.Headless,
		"protocolVersion": events.ProtocolVersion,
		"host":            session.GetHost(),
	})
}

func (h *WSHub) handleSendMessage(wc *wsConn, msgBytes []byte) {
	var req struct {
		SessionID string                 `json:"sessionId"`
		Text      string                 `json:"text"`
		Files     []types.FileAttachment `json:"files"`
	}
	json.Unmarshal(msgBytes, &req)

	if req.SessionID == "" {
		sendWSError(wc, "sessionId required")
		return
	}
	if err := h.sessions.SendMessage(req.SessionID, req.Text, req.Files); err != nil {
		sendWSError(wc, err.Error())
	}
}

func (h *WSHub) handleEndSession(wc *wsConn, msgBytes []byte) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(msgBytes, &req)

	if req.SessionID == "" {
		sendWSError(wc, "sessionId required")
		return
	}
	h.sessions.EndSession(req.SessionID)
}

func (h *WSHub) handleRenameSession(wc *wsConn, msgBytes []byte) {
	var req struct {
		SessionID string `json:"sessionId"`
		Name      string `json:"name"`
	}
	json.Unmarshal(msgBytes, &req)

	if req.SessionID == "" {
		sendWSError(wc, "sessionId required")
		return
	}
	if err := h.sessions.RenameSession(req.SessionID, req.Name); err != nil {
		sendWSError(wc, err.Error())
	}
}

func (h *WSHub) handleSetSessionFolder(wc *wsConn, msgBytes []byte) {
	var req struct {
		SessionID string `json:"sessionId"`
		Folder    string `json:"folder"`
	}
	json.Unmarshal(msgBytes, &req)

	if req.SessionID == "" {
		sendWSError(wc, "sessionId required")
		return
	}
	if err := h.sessions.SetSessionFolder(req.SessionID, req.Folder); err != nil {
		sendWSError(wc, err.Error())
	}
}

func (h *WSHub) handleDeleteSession(wc *wsConn, msgBytes []byte, boundSessions map[string]bool) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(msgBytes, &req)

	if req.SessionID == "" {
		sendWSError(wc, "sessionId required")
		return
	}
	h.sessions.DeleteSession(req.SessionID)

	if boundSessions[req.SessionID] {
		h.mu.Lock()
		removeViewer(h.conns, req.SessionID, wc)
		h.mu.Unlock()
		delete(boundSessions, req.SessionID)
	}

	// Notify all remaining viewers that the session was deleted.
	h.SendToSession(req.SessionID, map[string]interface{}{
		"type":      events.WSMsgSessionEnded,
		"sessionId": req.SessionID,
	})
	sendJSON(wc, map[string]interface{}{
		"type":      events.WSMsgSessionEnded,
		"sessionId": req.SessionID,
	})
}

func (h *WSHub) handleLeaveSession(wc *wsConn, msgBytes []byte, boundSessions map[string]bool) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(msgBytes, &req)

	if req.SessionID != "" && boundSessions[req.SessionID] {
		h.mu.Lock()
		removeViewer(h.conns, req.SessionID, wc)
		h.mu.Unlock()
		delete(boundSessions, req.SessionID)
	}
}

func (h *WSHub) handleStopGeneration(wc *wsConn, msgBytes []byte) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(msgBytes, &req)

	if req.SessionID == "" {
		sendWSError(wc, "sessionId required")
		return
	}
	if err := h.sessions.StopGeneration(req.SessionID); err != nil {
		sendWSError(wc, err.Error())
	}
}

func (h *WSHub) handleClearSession(wc *wsConn, msgBytes []byte) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(msgBytes, &req)

	if req.SessionID == "" {
		sendWSError(wc, "sessionId required")
		return
	}
	if err := h.sessions.ClearSession(req.SessionID); err != nil {
		sendWSError(wc, err.Error())
	}
}

// handleSetPermissionMode toggles a session's Claude permission mode mid-flight.
// Only Claude provider sessions support this — other providers ignore the
// request with an error event so the client can surface the regression.
func (h *WSHub) handleSetPermissionMode(wc *wsConn, msgBytes []byte) {
	var req struct {
		SessionID string `json:"sessionId"`
		Mode      string `json:"mode"`
	}
	json.Unmarshal(msgBytes, &req)

	if req.SessionID == "" {
		sendWSError(wc, "sessionId required")
		return
	}

	sess, ok := h.sessions.GetSession(req.SessionID)
	if !ok {
		sendWSError(wc, "session not found")
		return
	}

	if !types.CapabilitiesForProvider(sess.ProviderType).SupportsPermissions {
		sendWSError(wc, "permission mode toggle not supported for this provider")
		return
	}
	claude, ok := sess.Provider().(*provider.ClaudeProvider)
	if !ok {
		sendWSError(wc, "permission mode toggle not supported for this provider")
		return
	}

	if err := claude.SetPermissionMode(req.Mode); err != nil {
		sendWSError(wc, err.Error())
		return
	}

	sess.Lock()
	mode := sess.PermissionMode
	sess.Unlock()
	h.SendToSession(req.SessionID, map[string]interface{}{
		"type":      events.WSMsgModeChanged,
		"sessionId": req.SessionID,
		"mode":      mode,
	})
}

func (h *WSHub) handlePermissionResponse(msgBytes []byte) {
	var req struct {
		PermissionID string `json:"permissionId"`
		Approved     bool   `json:"approved"`
		Reason       string `json:"reason"`
	}
	json.Unmarshal(msgBytes, &req)

	decision := "deny"
	if req.Approved {
		decision = "allow"
	}
	h.perms.Resolve(req.PermissionID, permission.PermissionDecision{
		Decision: decision,
		Reason:   req.Reason,
	})
}

// --- Terminal message handlers ---

func (h *WSHub) handleTerminalCreate(wc *wsConn, msgBytes []byte, boundTerminals map[string]bool) {
	var req struct {
		TemplateID string   `json:"templateId"`
		Name       string   `json:"name"`
		Directory  string   `json:"directory"`
		ProjectID  string   `json:"projectId"`
		Cols       uint16   `json:"cols"`
		Rows       uint16   `json:"rows"`
		ExtraArgs  []string `json:"extraArgs"`
	}
	json.Unmarshal(msgBytes, &req)

	if req.TemplateID == "" {
		sendWSError(wc, "templateId required")
		return
	}

	session, err := h.terminals.Create(req.TemplateID, req.Name, req.Directory, req.ProjectID, req.Cols, req.Rows, req.ExtraArgs)
	if err != nil {
		sendWSError(wc, err.Error())
		return
	}

	h.joinTerminalConn(wc, session, boundTerminals)

	sendJSON(wc, map[string]interface{}{
		"type":       events.WSMsgTerminalCreated,
		"terminalId": session.ID,
		"templateId": session.TemplateID,
		"name":       session.Name,
		"directory":  session.Directory,
		"host":       session.Host.Chip(),
	})
}

func (h *WSHub) handleJoinTerminal(wc *wsConn, msgBytes []byte, boundTerminals map[string]bool) {
	var req struct {
		TerminalID string `json:"terminalId"`
	}
	json.Unmarshal(msgBytes, &req)
	if req.TerminalID == "" {
		return
	}

	session, ok := h.terminals.Get(req.TerminalID)
	if !ok {
		sendWSError(wc, "terminal not found: "+req.TerminalID)
		return
	}
	h.joinTerminalConn(wc, session, boundTerminals)
}

func (h *WSHub) handleLeaveTerminal(wc *wsConn, msgBytes []byte, boundTerminals map[string]bool) {
	var req struct {
		TerminalID string `json:"terminalId"`
	}
	json.Unmarshal(msgBytes, &req)

	if req.TerminalID == "" || !boundTerminals[req.TerminalID] {
		return
	}

	h.mu.Lock()
	remaining := removeViewer(h.termConns, req.TerminalID, wc)
	h.mu.Unlock()
	delete(boundTerminals, req.TerminalID)

	h.terminals.NotifyViewerChange(req.TerminalID, remaining)
}

func (h *WSHub) handleTerminalInput(wc *wsConn, msgBytes []byte) {
	var req struct {
		TerminalID string `json:"terminalId"`
		Data       string `json:"data"` // base64-encoded
	}
	json.Unmarshal(msgBytes, &req)

	if req.TerminalID == "" {
		return
	}

	decoded, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		sendWSError(wc, "invalid base64 data")
		return
	}
	if err := h.terminals.Write(req.TerminalID, decoded); err != nil {
		sendWSError(wc, err.Error())
	}
}

func (h *WSHub) handleTerminalResize(wc *wsConn, msgBytes []byte) {
	var req struct {
		TerminalID string `json:"terminalId"`
		Cols       uint16 `json:"cols"`
		Rows       uint16 `json:"rows"`
	}
	json.Unmarshal(msgBytes, &req)

	if req.TerminalID == "" {
		return
	}
	if err := h.terminals.Resize(req.TerminalID, req.Cols, req.Rows); err != nil {
		sendWSError(wc, err.Error())
	}
}

func (h *WSHub) handleTerminalClose(msgBytes []byte, boundTerminals map[string]bool) {
	var req struct {
		TerminalID string `json:"terminalId"`
	}
	json.Unmarshal(msgBytes, &req)

	if req.TerminalID == "" {
		return
	}

	h.terminals.Close(req.TerminalID)

	h.mu.Lock()
	delete(h.termConns, req.TerminalID)
	h.mu.Unlock()
	delete(boundTerminals, req.TerminalID)

	h.Broadcast(map[string]interface{}{
		"type":       events.WSMsgTerminalClosed,
		"terminalId": req.TerminalID,
	})
}

func (h *WSHub) handleTerminalList(wc *wsConn) {
	sendJSON(wc, map[string]interface{}{
		"type":      events.WSMsgTerminalList,
		"terminals": h.terminals.List(),
	})
}

func (h *WSHub) handleTerminalReconnect(wc *wsConn, msgBytes []byte, boundTerminals map[string]bool) {
	var req struct {
		TerminalID string `json:"terminalId"`
		Cols       uint16 `json:"cols"`
		Rows       uint16 `json:"rows"`
	}
	json.Unmarshal(msgBytes, &req)
	if req.TerminalID == "" {
		return
	}

	session, ok := h.terminals.Get(req.TerminalID)
	if !ok {
		sendWSError(wc, "terminal not found: "+req.TerminalID)
		return
	}

	// Match the PTY to the client's viewport before capturing scrollback.
	// This prevents the visible-duplicate-screen artifact: if we send
	// scrollback at the old size and the client then resizes, TUI apps
	// (Claude Code, etc.) repaint via SIGWINCH on top of the already-rendered
	// scrollback, leaving two copies of the UI in the buffer. Resizing first
	// means xterm and the bytes it replays agree on dimensions from the start.
	if req.Cols > 0 && req.Rows > 0 {
		curCols, curRows := session.Size()
		if req.Cols != curCols || req.Rows != curRows {
			if err := session.Resize(req.Cols, req.Rows); err != nil {
				slog.Debug("terminal reconnect resize failed", "id", req.TerminalID, "error", err)
			}
		}
	}

	h.joinTerminalConn(wc, session, boundTerminals)
}

func (h *WSHub) handleTerminalTemplates(wc *wsConn) {
	sendJSON(wc, map[string]interface{}{
		"type":      events.WSMsgTerminalTemplates,
		"templates": h.terminals.ListTemplates(),
	})
}

// --- Shared helpers ---

// Broadcast sends a message to all connected WebSocket clients,
// including those not currently bound to a session.
func (h *WSHub) Broadcast(msg map[string]interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		slog.Error("failed to marshal broadcast message", "error", err)
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.allConns {
		c.write(data)
	}
}

// joinTerminalConn binds a WS connection to a terminal and sends the join response with scrollback.
func (h *WSHub) joinTerminalConn(wc *wsConn, session *terminal.TerminalSession, boundTerminals map[string]bool) {
	tid := session.ID
	boundTerminals[tid] = true
	h.mu.Lock()
	addViewer(h.termConns, tid, wc)
	viewerCount := len(h.termConns[tid])
	h.mu.Unlock()

	// Cancel idle timer since a viewer connected.
	h.terminals.NotifyViewerChange(tid, viewerCount)

	scrollback := session.ScrollbackBytes()
	state, exitCode := session.Snapshot()
	cols, rows := session.Size()
	sendJSON(wc, map[string]interface{}{
		"type":       events.WSMsgTerminalJoined,
		"terminalId": tid,
		"templateId": session.TemplateID,
		"name":       session.Name,
		"directory":  session.Directory,
		"state":      state,
		"cols":       cols,
		"rows":       rows,
		"scrollback": base64.StdEncoding.EncodeToString(scrollback),
		"host":       session.Host.Chip(),
	})

	if state == "stopped" {
		sendJSON(wc, map[string]interface{}{
			"type":       events.WSMsgTerminalExit,
			"terminalId": tid,
			"exitCode":   exitCode,
		})
	}
}

// addViewer adds wc to a viewer set, initializing the set if needed.
// Caller must hold h.mu.
func addViewer(sets map[string]map[*wsConn]bool, id string, wc *wsConn) {
	if sets[id] == nil {
		sets[id] = make(map[*wsConn]bool)
	}
	sets[id][wc] = true
}

// removeViewer removes wc from a viewer set, cleaning up the set if empty.
// Returns the number of remaining viewers. Caller must hold h.mu.
func removeViewer(sets map[string]map[*wsConn]bool, id string, wc *wsConn) int {
	viewers, ok := sets[id]
	if !ok {
		return 0
	}
	delete(viewers, wc)
	if len(viewers) == 0 {
		delete(sets, id)
		return 0
	}
	return len(viewers)
}

func sendJSON(wc *wsConn, msg map[string]interface{}) {
	data, _ := json.Marshal(msg)
	wc.write(data)
}

func sendWSError(wc *wsConn, msg string) {
	sendJSON(wc, map[string]interface{}{
		"type":    events.WSMsgError,
		"message": msg,
	})
}
