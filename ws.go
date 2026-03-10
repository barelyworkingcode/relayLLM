package main

import (
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
	mu       sync.RWMutex
	conns    map[string]*wsConn // sessionID → connection
	sessions *SessionManager
	perms    *PermissionManager
}

type wsConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func NewWSHub(sessions *SessionManager, perms *PermissionManager) *WSHub {
	return &WSHub{
		conns:    make(map[string]*wsConn),
		sessions: sessions,
		perms:    perms,
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
	var boundSessionID string

	defer func() {
		if boundSessionID != "" {
			h.mu.Lock()
			delete(h.conns, boundSessionID)
			h.mu.Unlock()
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

			// Unbind previous session.
			if boundSessionID != "" {
				h.mu.Lock()
				delete(h.conns, boundSessionID)
				h.mu.Unlock()
			}

			boundSessionID = req.SessionID
			h.mu.Lock()
			h.conns[req.SessionID] = wc
			h.mu.Unlock()

			// Send session history.
			session.mu.Lock()
			history := make([]Message, len(session.Messages))
			copy(history, session.Messages)
			stats := session.Stats
			session.mu.Unlock()

			resp := map[string]interface{}{
				"type":      "session_joined",
				"sessionId": session.ID,
				"directory": session.Directory,
				"model":     session.Model,
				"name":      session.Name,
				"history":   history,
				"stats":     stats,
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
				sessionID = boundSessionID
			}
			if sessionID == "" {
				sendWSError(wc, "no session bound")
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
				sessionID = boundSessionID
			}
			if sessionID != "" {
				h.sessions.EndSession(sessionID)
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
		}
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
