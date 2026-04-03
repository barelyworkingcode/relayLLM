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
	conns     map[string]*wsConn          // sessionID → connection
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
		conns:     make(map[string]*wsConn),
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

// SendToSession implements EventSink.
func (h *WSHub) SendToSession(sessionID string, msg map[string]interface{}) {
	h.mu.RLock()
	c, ok := h.conns[sessionID]
	h.mu.RUnlock()

	if !ok {
		return
	}

	data, err := json.Marshal(msg)
	if err != nil {
		slog.Error("failed to marshal WS message", "error", err)
		return
	}

	c.mu.Lock()
	err = c.conn.WriteMessage(websocket.TextMessage, data)
	c.mu.Unlock()

	if err != nil {
		slog.Error("failed to write WS message", "session", sessionID, "error", err)
	}
}

// HandleUpgrade handles WebSocket upgrade requests.
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
			delete(h.conns, sid)
		}
		// Track terminals that lost their last viewer for idle timeout.
		var orphaned []string
		for tid := range boundTerminals {
			if viewers, ok := h.termConns[tid]; ok {
				delete(viewers, wc)
				if len(viewers) == 0 {
					delete(h.termConns, tid)
					orphaned = append(orphaned, tid)
				}
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
			var req struct {
				SessionID string `json:"sessionId"`
			}
			json.Unmarshal(msgBytes, &req)
			if req.SessionID == "" {
				continue
			}

			session, ok := h.sessions.GetSession(req.SessionID)
			if !ok {
				sendWSError(wc, "session not found: "+req.SessionID)
				continue
			}

			// Add binding (keep existing bindings for other sessions).
			boundSessions[req.SessionID] = true
			h.mu.Lock()
			h.conns[req.SessionID] = wc
			h.mu.Unlock()

			// Send session history.
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
			// Fall back to session.Messages (user-only) if Claude history unavailable.
			if history == nil {
				session.mu.Lock()
				history = make([]Message, len(session.Messages))
				copy(history, session.Messages)
				session.mu.Unlock()
			}
			session.mu.Lock()
			stats := session.Stats
			session.mu.Unlock()

			resp := map[string]interface{}{
				"type":      "session_joined",
				"sessionId": session.ID,
				"projectId": session.ProjectID,
				"directory": session.Directory,
				"model":     session.Model,
				"name":      session.Name,
				"history":   history,
				"stats":     stats,
				"headless":  session.Headless,
			}
			data, _ := json.Marshal(resp)
			wc.mu.Lock()
			wc.conn.WriteMessage(websocket.TextMessage, data)
			wc.mu.Unlock()

		case "send_message":
			var req struct {
				SessionID string           `json:"sessionId"`
				Text      string           `json:"text"`
				Files     []FileAttachment `json:"files"`
			}
			json.Unmarshal(msgBytes, &req)

			sessionID := req.SessionID
			if sessionID == "" {
				sendWSError(wc, "sessionId required")
				continue
			}

			if err := h.sessions.SendMessage(sessionID, req.Text, req.Files); err != nil {
				sendWSError(wc, err.Error())
			}

		case "end_session":
			var req struct {
				SessionID string `json:"sessionId"`
			}
			json.Unmarshal(msgBytes, &req)

			sessionID := req.SessionID
			if sessionID == "" {
				sendWSError(wc, "sessionId required")
				continue
			}
			h.sessions.EndSession(sessionID)

		case "rename_session":
			var req struct {
				SessionID string `json:"sessionId"`
				Name      string `json:"name"`
			}
			json.Unmarshal(msgBytes, &req)

			sessionID := req.SessionID
			if sessionID == "" {
				sendWSError(wc, "sessionId required")
				continue
			}
			if err := h.sessions.RenameSession(sessionID, req.Name); err != nil {
				sendWSError(wc, err.Error())
			}

		case "delete_session":
			var req struct {
				SessionID string `json:"sessionId"`
			}
			json.Unmarshal(msgBytes, &req)

			sessionID := req.SessionID
			if sessionID == "" {
				sendWSError(wc, "sessionId required")
				continue
			}
			h.sessions.DeleteSession(sessionID)
			// Unbind if this was a bound session
			if boundSessions[sessionID] {
				h.mu.Lock()
				delete(h.conns, sessionID)
				h.mu.Unlock()
				delete(boundSessions, sessionID)
			}
			// Notify the client
			resp := map[string]interface{}{
				"type":      "session_ended",
				"sessionId": sessionID,
			}
			data, _ := json.Marshal(resp)
			wc.mu.Lock()
			wc.conn.WriteMessage(websocket.TextMessage, data)
			wc.mu.Unlock()

		case "leave_session":
			var req struct {
				SessionID string `json:"sessionId"`
			}
			json.Unmarshal(msgBytes, &req)

			if req.SessionID != "" && boundSessions[req.SessionID] {
				h.mu.Lock()
				delete(h.conns, req.SessionID)
				h.mu.Unlock()
				delete(boundSessions, req.SessionID)
			}

		case "stop_generation":
			var req struct {
				SessionID string `json:"sessionId"`
			}
			json.Unmarshal(msgBytes, &req)

			sessionID := req.SessionID
			if sessionID == "" {
				sendWSError(wc, "sessionId required")
				continue
			}
			if err := h.sessions.StopGeneration(sessionID); err != nil {
				sendWSError(wc, err.Error())
			}

		case "clear_session":
			var req struct {
				SessionID string `json:"sessionId"`
			}
			json.Unmarshal(msgBytes, &req)

			sessionID := req.SessionID
			if sessionID == "" {
				sendWSError(wc, "sessionId required")
				continue
			}
			if err := h.sessions.ClearSession(sessionID); err != nil {
				sendWSError(wc, err.Error())
			}

		case "permission_response":
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

		// --- Terminal messages ---

		case "terminal_create":
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
				continue
			}

			session, err := h.terminals.Create(req.TemplateID, req.Name, req.Directory, req.Cols, req.Rows)
			if err != nil {
				sendWSError(wc, err.Error())
				continue
			}

			// Auto-join the creator and send terminal_created only to them.
			h.joinTerminalConn(wc, session, boundTerminals)

			resp := map[string]interface{}{
				"type":       "terminal_created",
				"terminalId": session.ID,
				"templateId": session.TemplateID,
				"name":       session.Name,
				"directory":  session.Directory,
			}
			data, _ := json.Marshal(resp)
			wc.mu.Lock()
			wc.conn.WriteMessage(websocket.TextMessage, data)
			wc.mu.Unlock()

		case "join_terminal":
			var req struct {
				TerminalID string `json:"terminalId"`
			}
			json.Unmarshal(msgBytes, &req)
			if req.TerminalID == "" {
				continue
			}

			session, ok := h.terminals.Get(req.TerminalID)
			if !ok {
				sendWSError(wc, "terminal not found: "+req.TerminalID)
				continue
			}

			h.joinTerminalConn(wc, session, boundTerminals)

		case "leave_terminal":
			var req struct {
				TerminalID string `json:"terminalId"`
			}
			json.Unmarshal(msgBytes, &req)

			if req.TerminalID != "" && boundTerminals[req.TerminalID] {
				h.mu.Lock()
				if viewers, ok := h.termConns[req.TerminalID]; ok {
					delete(viewers, wc)
					if len(viewers) == 0 {
						delete(h.termConns, req.TerminalID)
					}
				}
				h.mu.Unlock()
				delete(boundTerminals, req.TerminalID)
			}

		case "terminal_input":
			var req struct {
				TerminalID string `json:"terminalId"`
				Data       string `json:"data"` // base64-encoded
			}
			json.Unmarshal(msgBytes, &req)

			if req.TerminalID == "" {
				continue
			}

			decoded, err := base64.StdEncoding.DecodeString(req.Data)
			if err != nil {
				sendWSError(wc, "invalid base64 data")
				continue
			}

			if err := h.terminals.Write(req.TerminalID, decoded); err != nil {
				sendWSError(wc, err.Error())
			}

		case "terminal_resize":
			var req struct {
				TerminalID string `json:"terminalId"`
				Cols       uint16 `json:"cols"`
				Rows       uint16 `json:"rows"`
			}
			json.Unmarshal(msgBytes, &req)

			if req.TerminalID == "" {
				continue
			}

			if err := h.terminals.Resize(req.TerminalID, req.Cols, req.Rows); err != nil {
				sendWSError(wc, err.Error())
			}

		case "terminal_close":
			var req struct {
				TerminalID string `json:"terminalId"`
			}
			json.Unmarshal(msgBytes, &req)

			if req.TerminalID == "" {
				continue
			}

			h.terminals.Close(req.TerminalID)

			// Clean up viewer bindings.
			h.mu.Lock()
			delete(h.termConns, req.TerminalID)
			h.mu.Unlock()
			delete(boundTerminals, req.TerminalID)

			h.Broadcast(map[string]interface{}{
				"type":       "terminal_closed",
				"terminalId": req.TerminalID,
			})

		case "terminal_list":
			list := h.terminals.List()
			resp := map[string]interface{}{
				"type":      "terminal_list",
				"terminals": list,
			}
			data, _ := json.Marshal(resp)
			wc.mu.Lock()
			wc.conn.WriteMessage(websocket.TextMessage, data)
			wc.mu.Unlock()

		case "terminal_reconnect":
			var req struct {
				TerminalID string `json:"terminalId"`
			}
			json.Unmarshal(msgBytes, &req)
			if req.TerminalID == "" {
				continue
			}

			session, ok := h.terminals.Get(req.TerminalID)
			if !ok {
				sendWSError(wc, "terminal not found: "+req.TerminalID)
				continue
			}

			h.joinTerminalConn(wc, session, boundTerminals)

		case "terminal_templates":
			templates := h.terminals.ListTemplates()
			resp := map[string]interface{}{
				"type":      "terminal_templates",
				"templates": templates,
			}
			data, _ := json.Marshal(resp)
			wc.mu.Lock()
			wc.conn.WriteMessage(websocket.TextMessage, data)
			wc.mu.Unlock()
		}
	}
}

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
	if h.termConns[tid] == nil {
		h.termConns[tid] = make(map[*wsConn]bool)
	}
	h.termConns[tid][wc] = true
	viewerCount := len(h.termConns[tid])
	h.mu.Unlock()

	// Cancel idle timer since a viewer connected.
	h.terminals.NotifyViewerChange(tid, viewerCount)

	scrollback := session.ScrollbackBytes()
	state, exitCode := session.Snapshot()
	resp := map[string]interface{}{
		"type":       "terminal_joined",
		"terminalId": tid,
		"templateId": session.TemplateID,
		"name":       session.Name,
		"directory":  session.Directory,
		"state":      state,
		"scrollback": base64.StdEncoding.EncodeToString(scrollback),
	}
	data, _ := json.Marshal(resp)
	wc.mu.Lock()
	wc.conn.WriteMessage(websocket.TextMessage, data)
	wc.mu.Unlock()

	// If the terminal already exited, send exit event.
	if state == "stopped" {
		exitMsg := map[string]interface{}{
			"type":       "terminal_exit",
			"terminalId": tid,
			"exitCode":   exitCode,
		}
		exitData, _ := json.Marshal(exitMsg)
		wc.mu.Lock()
		wc.conn.WriteMessage(websocket.TextMessage, exitData)
		wc.mu.Unlock()
	}
}

func sendWSError(wc *wsConn, msg string) {
	data, _ := json.Marshal(map[string]string{
		"type":    "error",
		"message": msg,
	})
	wc.mu.Lock()
	wc.conn.WriteMessage(websocket.TextMessage, data)
	wc.mu.Unlock()
}
