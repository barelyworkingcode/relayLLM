package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
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
			McpToken       string          `json:"mcpToken"`
			Settings       json.RawMessage `json:"settings"`
		}
		if err := readJSON(r, &body); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid request body"})
			return
		}
		session, err := sessions.CreateSession(body.ProjectID, body.Directory, body.Name, body.Model, body.SystemPrompt, body.AppendClaudeMd, body.ProviderType, body.Settings, body.McpToken)
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

	mux.HandleFunc("PUT /api/sessions/{id}/model", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Provider string `json:"provider"`
			ModelID  string `json:"modelId"`
		}
		if err := readJSON(r, &body); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid request body"})
			return
		}
		if err := sessions.SetPiModel(r.PathValue("id"), body.Provider, body.ModelID); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]bool{"success": true})
	})

	mux.HandleFunc("PUT /api/sessions/{id}/thinking-level", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Level string `json:"level"`
		}
		if err := readJSON(r, &body); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid request body"})
			return
		}
		if err := sessions.SetPiThinkingLevel(r.PathValue("id"), body.Level); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]bool{"success": true})
	})
}

// --- Models Route ---

type ModelInfo struct {
	Label               string `json:"label"`
	Value               string `json:"value"`
	Group               string `json:"group"`
	Provider            string `json:"provider"`
	SupportsPermissions bool   `json:"supportsPermissions"`
	SupportsAttachments bool   `json:"supportsAttachments"`
}

func RegisterModelRoutes(mux *http.ServeMux, ollamaURL string, openaiCfg *OpenAIConfig, llamaMgr *LlamaServerManager, piCfg *PiConfig, piOverlay func() PiOverlayInputs) {
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
			pi     []ModelInfo
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
			ctx := r.Context()
			for i, endpoint := range openaiCfg.Endpoints {
				wg.Add(1)
				go func(i int, ep OpenAIEndpoint) {
					defer wg.Done()
					openai[i] = FetchOpenAIModels(ctx, ep)
				}(i, endpoint)
			}
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			var overlay PiOverlayInputs
			if piOverlay != nil {
				overlay = piOverlay()
			}
			pi = FetchPiModels(r.Context(), piCfg, overlay)
		}()

		wg.Wait()

		models := append(claude, ollama...)
		for _, ms := range openai {
			models = append(models, ms...)
		}
		if llamaMgr != nil {
			models = append(models, llamaMgr.ListModels()...)
		}
		models = append(models, pi...)

		// Stamp provider-default capabilities, OR-ing with any per-source
		// values (e.g. llama uses per-model mmproj detection).
		for i := range models {
			caps := CapabilitiesForProvider(models[i].Provider)
			models[i].SupportsPermissions = models[i].SupportsPermissions || caps.SupportsPermissions
			models[i].SupportsAttachments = models[i].SupportsAttachments || caps.SupportsAttachments
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
			TemplateID string   `json:"templateId"`
			Name       string   `json:"name"`
			Directory  string   `json:"directory"`
			Cols       uint16   `json:"cols"`
			Rows       uint16   `json:"rows"`
			ExtraArgs  []string `json:"extraArgs"`
		}
		if err := readJSON(r, &body); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid request body"})
			return
		}
		session, err := terminals.Create(body.TemplateID, body.Name, body.Directory, body.Cols, body.Rows, body.ExtraArgs)
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

	// Stitched head + tail of the PTY's raw byte stream. Works whether the
	// session is live, exited but still resident, or evicted from memory —
	// the files persist on disk until the log sweeper removes them.
	mux.HandleFunc("GET /api/terminals/{id}/log", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		dir := terminals.LogDir()
		if dir == "" {
			http.Error(w, "log persistence disabled", http.StatusNotFound)
			return
		}
		head, tail, err := openTerminalLogReaders(dir, id)
		if err != nil {
			if errors.Is(err, errTerminalLogNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		for _, f := range []*os.File{head, tail} {
			if f == nil {
				continue
			}
			if _, cerr := io.Copy(w, f); cerr != nil {
				slog.Debug("terminal log stream failed", "id", id, "error", cerr)
			}
			_ = f.Close()
		}
	})
}

// --- Permission Routes ---

func RegisterPermissionRoutes(mux *http.ServeMux, perms *PermissionManager, sessions *SessionManager) {
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

		slog.Info("permission request", "session", body.SessionID, "tool", body.ToolName, "toolUseId", body.ToolUseID)

		// Short-circuit if the session's policy matches a deny/allow rule.
		// Deny is checked first so it wins on overlap.
		if sess, ok := sessions.GetSession(body.SessionID); ok && sess.Policy != nil {
			if MatchToolRule(body.ToolName, body.ToolInput, sess.Policy.DeniedTools) {
				slog.Info("permission auto-denied by rule", "session", body.SessionID, "tool", body.ToolName)
				writeJSON(w, 200, PermissionDecision{Decision: "deny", Reason: "denied by project policy"})
				return
			}
			if MatchToolRule(body.ToolName, body.ToolInput, sess.Policy.AllowedTools) {
				slog.Info("permission auto-allowed by rule", "session", body.SessionID, "tool", body.ToolName)
				writeJSON(w, 200, PermissionDecision{Decision: "allow", Reason: "allowed by project policy"})
				return
			}
		}

		req, ch := perms.CreateRequest(body.SessionID, body.ToolName, body.ToolInput, body.ToolUseID)

		if perms.sink != nil {
			perms.sink.SendToSession(body.SessionID, map[string]interface{}{
				"type":         WSMsgPermissionRequest,
				"sessionId":    body.SessionID,
				"permissionId": req.ID,
				"toolName":     body.ToolName,
				"toolInput":    body.ToolInput,
				"toolUseId":    body.ToolUseID,
			})
		}

		select {
		case decision := <-ch:
			writeJSON(w, 200, decision)
		case <-perms.clock.After(60 * time.Second):
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

// Image generation is no longer an HTTP endpoint here — it is the
// relay-comfyui MCP tool, reached via `relay mcp call` (see ADR-006). Only the
// static serving route above remains; relayComfy writes images into
// {dataDir}/generated/ and Eve renders them from /api/generated/.

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
