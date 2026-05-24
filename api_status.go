package main

import (
	"net/http"
	"time"
)

// RegisterStatusRoutes wires GET /api/status, GET /api/llama/instances, and
// DELETE /api/llama/instances/{alias}. These are intended for the relay
// menubar's status panel and inherit the bearerAuth chain from main.go.
func RegisterStatusRoutes(
	mux *http.ServeMux,
	sessions *SessionManager,
	terminals *TerminalManager,
	llama *LlamaServerManager,
	startTime time.Time,
) {
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		instances := []LlamaInstanceInfo{}
		if llama != nil {
			instances = llama.ListInstances()
		}
		writeJSON(w, 200, map[string]interface{}{
			"uptimeSeconds": int64(time.Since(startTime).Seconds()),
			"sessions":      len(sessions.ListSessions()),
			"instances":     instances,
			"terminals":     terminals.ListSummary(),
		})
	})

	mux.HandleFunc("GET /api/llama/instances", func(w http.ResponseWriter, r *http.Request) {
		if llama == nil {
			writeJSON(w, 200, []LlamaInstanceInfo{})
			return
		}
		writeJSON(w, 200, llama.ListInstances())
	})

	mux.HandleFunc("DELETE /api/llama/instances/{alias}", func(w http.ResponseWriter, r *http.Request) {
		alias := r.PathValue("alias")
		if llama == nil {
			writeJSON(w, 404, map[string]string{"error": "llama manager not configured"})
			return
		}
		if err := llama.StopInstance(alias); err != nil {
			writeJSON(w, 404, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
