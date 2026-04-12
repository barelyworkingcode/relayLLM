package main

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

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
	sessions  *SessionManager
	perms     *PermissionManager
	terminals *TerminalManager
}

type wsConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func NewWSHub(sessions *SessionManager, perms *PermissionManager, terminals *TerminalManager) *WSHub {
	return &WSHub{
		conns:     make(map[string]map[*wsConn]bool),
		termConns: make(map[string]map[*wsConn]bool),
		allConns:  make(map[*wsConn]bool),
		sessions:  sessions,
		perms:     perms,
		terminals: terminals,
	}
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
		wc.mu.Lock()
		wc.conn.WriteMessage(websocket.TextMessage, data)
		wc.mu.Unlock()
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
		wc.mu.Lock()
		wc.conn.WriteMessage(websocket.TextMessage, data)
		wc.mu.Unlock()
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

	wc := &wsConn{conn: conn}
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

		var msg struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(msgBytes, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "join_session":
			h.handleJoinSession(wc, msgBytes, boundSessions)
		case "send_message":
			h.handleSendMessage(wc, msgBytes)
		case "end_session":
			h.handleEndSession(wc, msgBytes)
		case "rename_session":
			h.handleRenameSession(wc, msgBytes)
		case "delete_session":
			h.handleDeleteSession(wc, msgBytes, boundSessions)
		case "leave_session":
			h.handleLeaveSession(wc, msgBytes, boundSessions)
		case "stop_generation":
			h.handleStopGeneration(wc, msgBytes)
		case "clear_session":
			h.handleClearSession(wc, msgBytes)
		case "permission_response":
			h.handlePermissionResponse(msgBytes)
		case "terminal_create":
			h.handleTerminalCreate(wc, msgBytes, boundTerminals)
		case "join_terminal":
			h.handleJoinTerminal(wc, msgBytes, boundTerminals)
		case "leave_terminal":
			h.handleLeaveTerminal(wc, msgBytes, boundTerminals)
		case "terminal_input":
			h.handleTerminalInput(wc, msgBytes)
		case "terminal_resize":
			h.handleTerminalResize(wc, msgBytes)
		case "terminal_close":
			h.handleTerminalClose(msgBytes, boundTerminals)
		case "terminal_list":
			h.handleTerminalList(wc)
		case "terminal_reconnect":
			h.handleTerminalReconnect(wc, msgBytes, boundTerminals)
		case "terminal_templates":
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
	var history []Message
	var claudeSessionID string
	if session.ProviderState != nil {
		var ps struct {
			ClaudeSessionID string `json:"claudeSessionId"`
		}
		json.Unmarshal(session.ProviderState, &ps)
		claudeSessionID = ps.ClaudeSessionID
	}
	if claudeSessionID != "" {
		if h, err := readClaudeHistory(session.Directory, claudeSessionID); err == nil && len(h) > 0 {
			history = h
		} else if err != nil {
			slog.Debug("claude history unavailable, using session messages", "session", req.SessionID, "error", err)
		}
	}
	// Fall back to session.Messages if Claude history unavailable.
	if history == nil {
		session.mu.Lock()
		history = make([]Message, len(session.Messages))
		copy(history, session.Messages)
		session.mu.Unlock()
	}
	session.mu.Lock()
	stats := session.Stats
	session.mu.Unlock()

	sendJSON(wc, map[string]interface{}{
		"type":      "session_joined",
		"sessionId": session.ID,
		"projectId": session.ProjectID,
		"directory": session.Directory,
		"model":     session.Model,
		"name":      session.Name,
		"history":   history,
		"stats":     stats,
		"headless":  session.Headless,
	})
}

func (h *WSHub) handleSendMessage(wc *wsConn, msgBytes []byte) {
	var req struct {
		SessionID string           `json:"sessionId"`
		Text      string           `json:"text"`
		Files     []FileAttachment `json:"files"`
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
		"type":      "session_ended",
		"sessionId": req.SessionID,
	})
	sendJSON(wc, map[string]interface{}{
		"type":      "session_ended",
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
	h.perms.Resolve(req.PermissionID, PermissionDecision{
		Decision: decision,
		Reason:   req.Reason,
	})
}

// --- Terminal message handlers ---

func (h *WSHub) handleTerminalCreate(wc *wsConn, msgBytes []byte, boundTerminals map[string]bool) {
	var req struct {
		TemplateID string `json:"templateId"`
		Name       string `json:"name"`
		Directory  string `json:"directory"`
		Cols       uint16 `json:"cols"`
		Rows       uint16 `json:"rows"`
	}
	json.Unmarshal(msgBytes, &req)

	if req.TemplateID == "" {
		sendWSError(wc, "templateId required")
		return
	}

	session, err := h.terminals.Create(req.TemplateID, req.Name, req.Directory, req.Cols, req.Rows)
	if err != nil {
		sendWSError(wc, err.Error())
		return
	}

	h.joinTerminalConn(wc, session, boundTerminals)

	sendJSON(wc, map[string]interface{}{
		"type":       "terminal_created",
		"terminalId": session.ID,
		"templateId": session.TemplateID,
		"name":       session.Name,
		"directory":  session.Directory,
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
		"type":       "terminal_closed",
		"terminalId": req.TerminalID,
	})
}

func (h *WSHub) handleTerminalList(wc *wsConn) {
	sendJSON(wc, map[string]interface{}{
		"type":      "terminal_list",
		"terminals": h.terminals.List(),
	})
}

func (h *WSHub) handleTerminalReconnect(wc *wsConn, msgBytes []byte, boundTerminals map[string]bool) {
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

func (h *WSHub) handleTerminalTemplates(wc *wsConn) {
	sendJSON(wc, map[string]interface{}{
		"type":      "terminal_templates",
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
		c.mu.Lock()
		c.conn.WriteMessage(websocket.TextMessage, data)
		c.mu.Unlock()
	}
}

// joinTerminalConn binds a WS connection to a terminal and sends the join response with scrollback.
func (h *WSHub) joinTerminalConn(wc *wsConn, session *TerminalSession, boundTerminals map[string]bool) {
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
	sendJSON(wc, map[string]interface{}{
		"type":       "terminal_joined",
		"terminalId": tid,
		"templateId": session.TemplateID,
		"name":       session.Name,
		"directory":  session.Directory,
		"state":      state,
		"scrollback": base64.StdEncoding.EncodeToString(scrollback),
	})

	if state == "stopped" {
		sendJSON(wc, map[string]interface{}{
			"type":       "terminal_exit",
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
	wc.mu.Lock()
	wc.conn.WriteMessage(websocket.TextMessage, data)
	wc.mu.Unlock()
}

func sendWSError(wc *wsConn, msg string) {
	sendJSON(wc, map[string]interface{}{
		"type":    "error",
		"message": msg,
	})
}
