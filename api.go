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

func RegisterProjectRoutes(mux *http.ServeMux, store *ProjectStore, sc *SchedulerClient) {
	mux.HandleFunc("/api/projects", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, store.List())

		case http.MethodPost:
			var body struct {
				Name         string   `json:"name"`
				Path         string   `json:"path"`
				AllowedTools []string `json:"allowedTools"`
			}
			if err := readJSON(r, &body); err != nil {
				writeJSON(w, 400, map[string]string{"error": "invalid request body"})
				return
			}
			p, err := store.Create(body.Name, body.Path, body.AllowedTools)
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
			// Cascade: delete tasks for this project in scheduler
			if sc != nil {
				if _, _, err := sc.Proxy("DELETE", "/api/tasks/by-project/"+id, "", nil); err != nil {
					slog.Warn("scheduler cascade delete failed", "project", id, "error", err)
				}
			}
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
				ProjectID      string          `json:"projectId"`
				Directory      string          `json:"directory"`
				Name           string          `json:"name"`
				Model          string          `json:"model"`
				SystemPrompt   string          `json:"systemPrompt"`
				AppendClaudeMd bool            `json:"appendClaudeMd"`
				Settings       json.RawMessage `json:"settings"`
			}
			if err := readJSON(r, &body); err != nil {
				writeJSON(w, 400, map[string]string{"error": "invalid request body"})
				return
			}
			session, err := sessions.CreateSession(body.ProjectID, body.Directory, body.Name, body.Model, body.SystemPrompt, body.AppendClaudeMd, body.Settings)
			if err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, 201, map[string]interface{}{
				"sessionId": session.ID,
				"projectId": session.ProjectID,
				"directory": session.Directory,
				"model":     session.Model,
				"name":      session.Name,
			})

		default:
			w.WriteHeader(405)
		}
	})

	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
		parts := strings.SplitN(path, "/", 2)
		sessionID := parts[0]

		if len(parts) == 2 && parts[1] == "stop" && r.Method == http.MethodPost {
			// POST /api/sessions/:id/stop — abort in-flight generation
			if err := sessions.StopGeneration(sessionID); err != nil {
				writeJSON(w, 404, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, 200, map[string]bool{"success": true})
			return
		}

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

		if len(parts) == 2 && parts[1] == "delete" && r.Method == http.MethodPost {
			sessions.DeleteSession(sessionID)
			writeJSON(w, 200, map[string]bool{"success": true})
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

// --- Models Route ---

type ModelInfo struct {
	Label    string `json:"label"`
	Value    string `json:"value"`
	Group    string `json:"group"`
	Provider string `json:"provider"`
}

func RegisterModelRoutes(mux *http.ServeMux, lmStudioURL string) {
	mux.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}

		models := []ModelInfo{
			{Label: "Claude Haiku", Value: "haiku", Group: "Claude", Provider: "claude"},
			{Label: "Claude Sonnet", Value: "sonnet", Group: "Claude", Provider: "claude"},
			{Label: "Claude Opus", Value: "opus", Group: "Claude", Provider: "claude"},
		}

		if lmStudioURL != "" {
			if lmModels := fetchLMStudioModels(lmStudioURL); len(lmModels) > 0 {
				models = append(models, lmModels...)
			}
		}

		writeJSON(w, 200, map[string]interface{}{
			"models":           models,
			"providerSettings": ProviderSettings(),
		})
	})
}

// --- Terminal Routes ---

func RegisterTerminalRoutes(mux *http.ServeMux, templates *TemplateStore, terminals *TerminalManager) {
	// Template CRUD.
	mux.HandleFunc("/api/terminal/templates", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, templates.List())

		case http.MethodPost:
			var body struct {
				Name        string            `json:"name"`
				Command     string            `json:"command"`
				Args        []string          `json:"args"`
				Env         map[string]string `json:"env"`
				Description string            `json:"description"`
				Icon        string            `json:"icon"`
			}
			if err := readJSON(r, &body); err != nil {
				writeJSON(w, 400, map[string]string{"error": "invalid request body"})
				return
			}
			tmpl, err := templates.Create(TerminalTemplate{
				Name:        body.Name,
				Command:     body.Command,
				Args:        body.Args,
				Env:         body.Env,
				Description: body.Description,
				Icon:        body.Icon,
			})
			if err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, 201, tmpl)

		default:
			w.WriteHeader(405)
		}
	})

	mux.HandleFunc("/api/terminal/templates/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/terminal/templates/")
		if id == "" {
			w.WriteHeader(404)
			return
		}

		switch r.Method {
		case http.MethodGet:
			tmpl, ok := templates.Get(id)
			if !ok {
				writeJSON(w, 404, map[string]string{"error": "template not found"})
				return
			}
			writeJSON(w, 200, tmpl)

		case http.MethodPut:
			var updates map[string]interface{}
			if err := readJSON(r, &updates); err != nil {
				writeJSON(w, 400, map[string]string{"error": "invalid request body"})
				return
			}
			tmpl, err := templates.Update(id, updates)
			if err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, 200, tmpl)

		case http.MethodDelete:
			if err := templates.Delete(id); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, 200, map[string]bool{"success": true})

		default:
			w.WriteHeader(405)
		}
	})

	// Terminal instance CRUD.
	mux.HandleFunc("/api/terminals", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, terminals.List())

		case http.MethodPost:
			var body struct {
				TemplateID string `json:"templateId"`
				Name       string `json:"name"`
				Directory  string `json:"directory"`
				Cols       uint16 `json:"cols"`
				Rows       uint16 `json:"rows"`
			}
			if err := readJSON(r, &body); err != nil {
				writeJSON(w, 400, map[string]string{"error": "invalid request body"})
				return
			}
			session, err := terminals.Create(body.TemplateID, body.Name, body.Directory, body.Cols, body.Rows)
			if err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			state, _ := session.Snapshot()
			writeJSON(w, 201, map[string]interface{}{
				"id":         session.ID,
				"templateId": session.TemplateID,
				"name":       session.Name,
				"directory":  session.Directory,
				"state":      state,
			})

		default:
			w.WriteHeader(405)
		}
	})

	mux.HandleFunc("/api/terminals/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/terminals/")
		if id == "" {
			w.WriteHeader(404)
			return
		}

		switch r.Method {
		case http.MethodDelete:
			terminals.Close(id)
			writeJSON(w, 200, map[string]bool{"success": true})

		default:
			w.WriteHeader(405)
		}
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
