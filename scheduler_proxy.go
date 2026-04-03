package main

import (
	"log/slog"
	"net/http"
)

// RegisterSchedulerProxyRoutes registers task API routes that proxy to relayScheduler.
func RegisterSchedulerProxyRoutes(mux *http.ServeMux, sc *SchedulerClient) {
	mux.HandleFunc("GET /api/tasks", func(w http.ResponseWriter, r *http.Request) {
		proxyScheduler(w, r, sc, "GET", "/api/tasks", r.URL.RawQuery)
	})
	mux.HandleFunc("POST /api/tasks", func(w http.ResponseWriter, r *http.Request) {
		proxyScheduler(w, r, sc, "POST", "/api/tasks", "")
	})

	mux.HandleFunc("GET /api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		proxyScheduler(w, r, sc, "GET", "/api/tasks/"+r.PathValue("id"), "")
	})
	mux.HandleFunc("PUT /api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		proxyScheduler(w, r, sc, "PUT", "/api/tasks/"+r.PathValue("id"), "")
	})
	mux.HandleFunc("DELETE /api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		proxyScheduler(w, r, sc, "DELETE", "/api/tasks/"+r.PathValue("id"), "")
	})

	mux.HandleFunc("GET /api/tasks/{id}/history", func(w http.ResponseWriter, r *http.Request) {
		proxyScheduler(w, r, sc, "GET", "/api/tasks/"+r.PathValue("id")+"/history", "")
	})
	mux.HandleFunc("POST /api/tasks/{id}/run", func(w http.ResponseWriter, r *http.Request) {
		proxyScheduler(w, r, sc, "POST", "/api/tasks/"+r.PathValue("id")+"/run", "")
	})

	mux.HandleFunc("DELETE /api/tasks/by-project/{pid}", func(w http.ResponseWriter, r *http.Request) {
		proxyScheduler(w, r, sc, "DELETE", "/api/tasks/by-project/"+r.PathValue("pid"), "")
	})
}

func proxyScheduler(w http.ResponseWriter, r *http.Request, sc *SchedulerClient, method, path, query string) {
	status, data, err := sc.Proxy(method, path, query, r.Body)
	if err != nil {
		slog.Error("scheduler proxy error", "method", method, "path", path, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		w.Write([]byte(`{"error":"scheduler unavailable"}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(data)
}
