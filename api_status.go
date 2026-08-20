package main

import (
	"net/http"
	"time"
)

// RegisterStatusRoutes wires GET /api/status, GET /api/llama/instances,
// DELETE /api/llama/instances/{alias}, and their mlx counterparts. These are
// intended for the relay menubar's status panel and inherit the bearerAuth
// chain from main.go.
func RegisterStatusRoutes(
	mux *http.ServeMux,
	sessions *SessionManager,
	terminals *TerminalManager,
	llama *ServerManager,
	mlx *ServerManager,
	startTime time.Time,
) {
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		instances := []ServerInstanceInfo{}
		if llama != nil {
			instances = llama.ListInstances()
		}
		mlxInstances := []ServerInstanceInfo{}
		if mlx != nil {
			mlxInstances = mlx.ListInstances()
		}
		// Budgets are reported per configured manager so the Service Inspector
		// can show why a model is loaded, queued, or evicted.
		budgets := []BudgetInfo{}
		for _, mgr := range []*ServerManager{llama, mlx} {
			if mgr != nil {
				budgets = append(budgets, mgr.Budget())
			}
		}
		writeJSON(w, 200, map[string]interface{}{
			"uptimeSeconds": int64(time.Since(startTime).Seconds()),
			"sessions":      len(sessions.ListSessions()),
			"instances":     instances,
			"mlxInstances":  mlxInstances,
			"budgets":       budgets,
			"terminals":     terminals.ListSummary(),
		})
	})

	registerInstanceRoutes(mux, "llama", llama)
	registerInstanceRoutes(mux, "mlx", mlx)
}

// registerInstanceRoutes wires GET /api/{kind}/instances and
// DELETE /api/{kind}/instances/{alias} for one managed-server kind. The
// routes stay live with a nil manager: GET returns an empty list, DELETE 404s.
func registerInstanceRoutes(mux *http.ServeMux, kind string, mgr *ServerManager) {
	mux.HandleFunc("GET /api/"+kind+"/instances", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, 200, []ServerInstanceInfo{})
			return
		}
		writeJSON(w, 200, mgr.ListInstances())
	})

	mux.HandleFunc("DELETE /api/"+kind+"/instances/{alias}", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, 404, map[string]string{"error": kind + " manager not configured"})
			return
		}
		if err := mgr.StopInstance(r.PathValue("alias")); err != nil {
			writeJSON(w, 404, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
