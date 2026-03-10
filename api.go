package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v interface{}) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// --- Project Routes ---

func RegisterProjectRoutes(mux *http.ServeMux, store *ProjectStore) {
	mux.HandleFunc("/api/projects", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, store.List())

		case http.MethodPost:
			var body struct {
				Name         string   `json:"name"`
				Path         string   `json:"path"`
				Model        string   `json:"model"`
				AllowedTools []string `json:"allowedTools"`
			}
			if err := readJSON(r, &body); err != nil {
				writeJSON(w, 400, map[string]string{"error": "invalid request body"})
				return
			}
			p, err := store.Create(body.Name, body.Path, body.Model, body.AllowedTools)
			if err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, 201, p)

		default:
			w.WriteHeader(405)
		}
	})

	mux.HandleFunc("/api/projects/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/projects/")
		if id == "" {
			w.WriteHeader(404)
			return
		}

		switch r.Method {
		case http.MethodGet:
			p, ok := store.Get(id)
			if !ok {
				writeJSON(w, 404, map[string]string{"error": "project not found"})
				return
			}
			writeJSON(w, 200, p)

		case http.MethodPut:
			var updates map[string]interface{}
			if err := readJSON(r, &updates); err != nil {
				writeJSON(w, 400, map[string]string{"error": "invalid request body"})
				return
			}
			p, err := store.Update(id, updates)
			if err != nil {
				writeJSON(w, 404, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, 200, p)

		case http.MethodDelete:
			if err := store.Delete(id); err != nil {
				writeJSON(w, 404, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, 200, map[string]bool{"success": true})

		default:
			w.WriteHeader(405)
		}
	})
}

// --- Session Routes ---

func RegisterSessionRoutes(mux *http.ServeMux, sessions *SessionManager) {
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, sessions.ListSessions())

		case http.MethodPost:
			var body struct {
				ProjectID string `json:"projectId"`
				Name      string `json:"name"`
				Model     string `json:"model"`
			}
			if err := readJSON(r, &body); err != nil {
				writeJSON(w, 400, map[string]string{"error": "invalid request body"})
				return
			}
			session, err := sessions.CreateSession(body.ProjectID, body.Name, body.Model)
			if err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, 201, map[string]string{
				"sessionId": session.ID,
				"projectId": session.ProjectID,
				"model":     session.Model,
			})

		default:
			w.WriteHeader(405)
		}
	})

	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
		parts := strings.SplitN(path, "/", 2)
		sessionID := parts[0]

		if len(parts) == 2 && parts[1] == "message" && r.Method == http.MethodPost {
			// POST /api/sessions/:id/message — synchronous message (for HTTP clients)
			var body struct {
				Text  string           `json:"text"`
				Files []FileAttachment `json:"files"`
			}
			if err := readJSON(r, &body); err != nil {
				writeJSON(w, 400, map[string]string{"error": "invalid request body"})
				return
			}
			if body.Text == "" {
				writeJSON(w, 400, map[string]string{"error": "text is required"})
				return
			}

			response, stats, err := sessions.SendMessageSync(sessionID, body.Text, body.Files)
			if err != nil {
				if strings.Contains(err.Error(), "timeout") {
					writeJSON(w, 504, map[string]string{"error": err.Error()})
				} else {
					writeJSON(w, 500, map[string]string{"error": err.Error()})
				}
				return
			}

			writeJSON(w, 200, map[string]interface{}{
				"response": response,
				"stats":    stats,
			})
			return
		}

		if r.Method == http.MethodDelete {
			sessions.EndSession(sessionID)
			writeJSON(w, 200, map[string]bool{"success": true})
			return
		}

		w.WriteHeader(405)
	})
}

// --- Permission Routes ---

func RegisterPermissionRoutes(mux *http.ServeMux, perms *PermissionManager) {
	mux.HandleFunc("/api/permission", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}

		var body struct {
			SessionID string `json:"sessionId"`
			ToolName  string `json:"toolName"`
			ToolInput string `json:"toolInput"`
			ToolUseID string `json:"toolUseId"`
		}
		if err := readJSON(r, &body); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid request body"})
			return
		}

		slog.Info("permission request", "session", body.SessionID, "tool", body.ToolName)

		req, ch := perms.CreateRequest(body.SessionID, body.ToolName, body.ToolInput)

		// Push permission_request to the WebSocket client for this session.
		if perms.sink != nil {
			perms.sink.SendToSession(body.SessionID, map[string]interface{}{
				"type":         "permission_request",
				"sessionId":    body.SessionID,
				"permissionId": req.ID,
				"toolName":     body.ToolName,
				"toolInput":    body.ToolInput,
			})
		}

		// Hold the HTTP response open until the client responds or timeout.
		select {
		case decision := <-ch:
			writeJSON(w, 200, decision)
		case <-time.After(60 * time.Second):
			perms.Cleanup(req.ID)
			writeJSON(w, 200, PermissionDecision{Decision: "deny", Reason: "timeout"})
		}
	})
}
