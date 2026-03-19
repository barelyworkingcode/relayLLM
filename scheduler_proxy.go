package main

import (
	"log/slog"
	"net/http"
	"strings"
)

// RegisterSchedulerProxyRoutes registers task API routes that proxy to relayScheduler.
func RegisterSchedulerProxyRoutes(mux *http.ServeMux, sc *SchedulerClient) {
	// GET/POST /api/tasks
	mux.HandleFunc("/api/tasks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			proxyScheduler(w, r, sc, "GET", "/api/tasks", r.URL.RawQuery)
		case http.MethodPost:
			proxyScheduler(w, r, sc, "POST", "/api/tasks", "")
		default:
			w.WriteHeader(405)
		}
	})

	// /api/tasks/* routes
	mux.HandleFunc("/api/tasks/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
		if path == "" {
			w.WriteHeader(404)
			return
		}

		// DELETE /api/tasks/by-project/{pid}
		if strings.HasPrefix(path, "by-project/") {
			if r.Method != http.MethodDelete {
				w.WriteHeader(405)
				return
			}
			proxyScheduler(w, r, sc, "DELETE", r.URL.Path, "")
			return
		}

		// path = "{id}" or "{id}/history" or "{id}/run"
		parts := strings.SplitN(path, "/", 2)
		taskID := parts[0]

		if len(parts) == 1 {
			// GET/PUT/DELETE /api/tasks/{id}
			switch r.Method {
			case http.MethodGet:
				proxyScheduler(w, r, sc, "GET", "/api/tasks/"+taskID, "")
			case http.MethodPut:
				proxyScheduler(w, r, sc, "PUT", "/api/tasks/"+taskID, "")
			case http.MethodDelete:
				proxyScheduler(w, r, sc, "DELETE", "/api/tasks/"+taskID, "")
			default:
				w.WriteHeader(405)
			}
			return
		}

		sub := parts[1]
		switch sub {
		case "history":
			if r.Method != http.MethodGet {
				w.WriteHeader(405)
				return
			}
			proxyScheduler(w, r, sc, "GET", "/api/tasks/"+taskID+"/history", "")

		case "run":
			if r.Method != http.MethodPost {
				w.WriteHeader(405)
				return
			}
			proxyScheduler(w, r, sc, "POST", "/api/tasks/"+taskID+"/run", "")

		default:
			w.WriteHeader(404)
		}
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
