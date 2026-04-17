package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
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

// recoverMiddleware catches panics in HTTP handlers and returns 500
// instead of crashing the server.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("panic in HTTP handler", "error", err, "method", r.Method, "path", r.URL.Path)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// --- Project Routes ---

func RegisterProjectRoutes(mux *http.ServeMux, store *ProjectStore, sc *SchedulerClient) {
	mux.HandleFunc("GET /api/projects", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, store.List())
	})

	mux.HandleFunc("POST /api/projects", func(w http.ResponseWriter, r *http.Request) {
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
	})

	mux.HandleFunc("GET /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := store.Get(r.PathValue("id"))
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "project not found"})
			return
		}
		writeJSON(w, 200, p)
	})

	mux.HandleFunc("PUT /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		var updates ProjectUpdate
		if err := readJSON(r, &updates); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid request body"})
			return
		}
		p, err := store.Update(r.PathValue("id"), updates)
		if err != nil {
			writeJSON(w, 404, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, p)
	})

	mux.HandleFunc("DELETE /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
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
	})
}

// --- Session Routes ---

func RegisterSessionRoutes(mux *http.ServeMux, sessions *SessionManager) {
	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, sessions.ListSessions())
	})

	mux.HandleFunc("POST /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ProjectID      string          `json:"projectId"`
			Directory      string          `json:"directory"`
			Name           string          `json:"name"`
			Model          string          `json:"model"`
			SystemPrompt   string          `json:"systemPrompt"`
			AppendClaudeMd bool            `json:"appendClaudeMd"`
			ProviderType   string          `json:"providerType"`
			Settings       json.RawMessage `json:"settings"`
		}
		if err := readJSON(r, &body); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid request body"})
			return
		}
		session, err := sessions.CreateSession(body.ProjectID, body.Directory, body.Name, body.Model, body.SystemPrompt, body.AppendClaudeMd, body.ProviderType, body.Settings)
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
	})

	mux.HandleFunc("POST /api/sessions/{id}/message", func(w http.ResponseWriter, r *http.Request) {
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

		response, stats, err := sessions.SendMessageSync(r.PathValue("id"), body.Text, body.Files)
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
	})

	mux.HandleFunc("POST /api/sessions/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		if err := sessions.StopGeneration(r.PathValue("id")); err != nil {
			writeJSON(w, 404, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]bool{"success": true})
	})

	mux.HandleFunc("POST /api/sessions/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
		sessions.DeleteSession(r.PathValue("id"))
		writeJSON(w, 200, map[string]bool{"success": true})
	})

	mux.HandleFunc("DELETE /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		sessions.EndSession(r.PathValue("id"))
		writeJSON(w, 200, map[string]bool{"success": true})
	})
}

// --- Models Route ---

type ModelInfo struct {
	Label    string `json:"label"`
	Value    string `json:"value"`
	Group    string `json:"group"`
	Provider string `json:"provider"`
}

func RegisterModelRoutes(mux *http.ServeMux, ollamaURL string, openaiCfg *OpenAIConfig) {
	mux.HandleFunc("GET /api/models", func(w http.ResponseWriter, r *http.Request) {
		claude := []ModelInfo{
			{Label: "Claude Haiku", Value: "haiku", Group: "Claude", Provider: "claude"},
			{Label: "Claude Sonnet", Value: "sonnet", Group: "Claude", Provider: "claude"},
			{Label: "Claude Opus", Value: "opus", Group: "Claude", Provider: "claude"},
		}

		// Fan out discovery calls concurrently so one slow endpoint doesn't
		// block the others. Each goroutine writes to a distinct result slot,
		// so wg.Wait() is sufficient synchronization.
		var (
			wg     sync.WaitGroup
			ollama []ModelInfo
			openai [][]ModelInfo
		)

		if ollamaURL != "" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ollama = fetchOllamaModels(ollamaURL)
			}()
		}

		if openaiCfg != nil {
			openai = make([][]ModelInfo, len(openaiCfg.Endpoints))
			for i, endpoint := range openaiCfg.Endpoints {
				wg.Add(1)
				go func(i int, ep OpenAIEndpoint) {
					defer wg.Done()
					openai[i] = FetchOpenAIModels(ep)
				}(i, endpoint)
			}
		}
		wg.Wait()

		models := append(claude, ollama...)
		for _, ms := range openai {
			models = append(models, ms...)
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
	mux.HandleFunc("GET /api/terminal/templates", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, templates.List())
	})

	mux.HandleFunc("POST /api/terminal/templates", func(w http.ResponseWriter, r *http.Request) {
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
	})

	mux.HandleFunc("GET /api/terminal/templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		tmpl, ok := templates.Get(r.PathValue("id"))
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "template not found"})
			return
		}
		writeJSON(w, 200, tmpl)
	})

	mux.HandleFunc("PUT /api/terminal/templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		var updates TemplateUpdate
		if err := readJSON(r, &updates); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid request body"})
			return
		}
		tmpl, err := templates.Update(r.PathValue("id"), updates)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, tmpl)
	})

	mux.HandleFunc("DELETE /api/terminal/templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := templates.Delete(r.PathValue("id")); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]bool{"success": true})
	})

	// Terminal instance routes.
	mux.HandleFunc("GET /api/terminals", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, terminals.List())
	})

	mux.HandleFunc("POST /api/terminals", func(w http.ResponseWriter, r *http.Request) {
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
	})

	mux.HandleFunc("DELETE /api/terminals/{id}", func(w http.ResponseWriter, r *http.Request) {
		terminals.Close(r.PathValue("id"))
		writeJSON(w, 200, map[string]bool{"success": true})
	})
}

// --- Permission Routes ---

func RegisterPermissionRoutes(mux *http.ServeMux, perms *PermissionManager) {
	mux.HandleFunc("POST /api/permission", func(w http.ResponseWriter, r *http.Request) {
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

		if perms.sink != nil {
			perms.sink.SendToSession(body.SessionID, map[string]interface{}{
				"type":         "permission_request",
				"sessionId":    body.SessionID,
				"permissionId": req.ID,
				"toolName":     body.ToolName,
				"toolInput":    body.ToolInput,
			})
		}

		select {
		case decision := <-ch:
			writeJSON(w, 200, decision)
		case <-time.After(60 * time.Second):
			perms.Cleanup(req.ID)
			writeJSON(w, 200, PermissionDecision{Decision: "deny", Reason: "timeout"})
		}
	})
}

// RegisterGeneratedImageRoutes serves generated images (from ComfyUI) out of
// {dataDir}/generated/. Filenames are validated to prevent path traversal.
func RegisterGeneratedImageRoutes(mux *http.ServeMux, dataDir string) {
	generatedDir := filepath.Join(dataDir, "generated")
	mux.HandleFunc("GET /api/generated/{filename}", func(w http.ResponseWriter, r *http.Request) {
		filename := r.PathValue("filename")
		if !isValidGeneratedFilename(filename) {
			http.Error(w, "invalid filename", http.StatusBadRequest)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeFile(w, r, filepath.Join(generatedDir, filename))
	})
}

func isValidGeneratedFilename(name string) bool {
	if len(name) == 0 || len(name) > 255 {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}
