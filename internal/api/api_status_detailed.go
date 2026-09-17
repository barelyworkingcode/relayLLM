package api

// GET /api/status/detailed and GET /status — the diagnostic status
// dashboard's JSON feed and its embedded HTML shell. Deliberately separate
// from api_status.go's GET /api/status: that payload is relay's Service
// Inspector contract and must stay byte-identical (see CLAUDE.md's
// Relay-router section and api_status.go's own doc comment) — this is a
// different, additive endpoint for a different consumer, so it is free to
// carry a much richer shape.
//
// buildDetailedStatus takes each subsystem's own snapshot independently and
// in no particular order (ProxyMetrics.Snapshot, ProxyRegistry.Snapshot,
// ServerManager.ListInstances/ModelCatalog/Budget) — it never holds two
// subsystems' locks at once, since every call here is a "snapshot()-shaped,
// returns a copy" call.

import (
	"context"
	"embed"
	"net/http"
	clk "relayllm/internal/clock"
	"relayllm/internal/config"
	"relayllm/internal/registry"
	"relayllm/internal/router"
	"relayllm/internal/servermanager"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed status
var statusAssets embed.FS

// statusFileServer serves status/status.css and status/status.js. The
// embedded FS keeps the "status/" directory name as part of every path
// (go:embed on a directory pattern does not strip it), which happens to
// match the "/status/" URL prefix these are mounted under exactly — so no
// http.StripPrefix is needed.
var statusFileServer = http.FileServerFS(statusAssets)

// DetailedStatusDeps are the subsystems GET /api/status/detailed aggregates.
// Every field is nil-safe to omit: a nil Router yields overview.router.enabled
// == false and an empty proxy-connections/virtual section; a nil Registry
// yields an empty endpoints array; nil Managers and Virtual behave the same
// way. Clock nil -> DefaultClock.
//
// Router is non-nil, with router.enabled == true, whenever relay launched
// this process (C9) even with --router-port unset: router.sock (C9) needs a
// live router.RelayRouter regardless of TCP. overview.router.tcp and .socket
// distinguish the two transports — enabled alone can no longer be read as
// "TCP is bound", since a socket-only router reports enabled: true with an
// empty addr/addrs.
type DetailedStatusDeps struct {
	Managers  []*servermanager.ServerManager // dispatch priority order: llama, then mlx
	Registry  *registry.ProxyRegistry        // may be nil
	Virtual   *config.VirtualLLMConfig
	Router    *router.RelayRouter // may be nil (standalone with nothing to serve)
	StartTime time.Time
	Clock     clk.Clock
}

// RegisterDetailedStatusRoutes wires GET /api/status/detailed and the
// embedded GET /status dashboard (plus its /status/*.css /status/*.js
// assets) onto the main mux. Deliberately NOT the relay-router's TCP
// listener (see J8 in backend.md's design notes): that listener is
// unauthenticated by design (a local OpenAI-compatible endpoint), and this
// page exposes model config and endpoint names. The main mux sits behind
// bearerAuth on the Unix socket; a browser reaches /status only through the
// optional --http-port TCP front (see main_http_listener_test.go), which
// carries no bearer requirement of its own and is gated purely by which
// --http-bind addresses are actually bound.
func RegisterDetailedStatusRoutes(mux *http.ServeMux, deps DetailedStatusDeps) {
	mux.HandleFunc("GET /api/status/detailed", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, buildDetailedStatus(r.Context(), deps))
	})

	mux.HandleFunc("GET /status", serveStatusHTML)
	mux.HandleFunc("GET /status/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status/" {
			serveStatusHTML(w, r)
			return
		}
		// status.css/status.js change only on deploy, unlike the HTML shell
		// (which must always reflect the live-poll page, hence no-store) —
		// safe to let the browser cache these normally.
		w.Header().Set("Cache-Control", "public, max-age=3600")
		statusFileServer.ServeHTTP(w, r)
	})
}

func serveStatusHTML(w http.ResponseWriter, r *http.Request) {
	data, err := statusAssets.ReadFile("status/status.html")
	if err != nil {
		http.Error(w, "status page unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

// buildDetailedStatus assembles the full JSON document as a plain map,
// matching RegisterStatusRoutes' existing style so the handler stays
// trivially testable. ctx is passed into the endpoint registry's Snapshot —
// see ProxyRegistry.probe's context.WithoutCancel comment for why a
// dashboard tab closing mid-poll cannot poison the reachability cache.
func buildDetailedStatus(ctx context.Context, deps DetailedStatusDeps) map[string]any {
	clock := deps.Clock
	if clock == nil {
		clock = clk.DefaultClock
	}
	now := clock.Now()

	proxyInfos, proxyAgg, recentReqs := deps.Router.Metrics().Snapshot()

	// Bucket each in-flight proxy connection by what it's actually hitting —
	// one pass, shared by models.instances/virtual/endpoints' activeRequests
	// fields below. Keys mirror exactly what routeManaged/routeOpenAI/
	// routeVirtual store in ProxyConn.target via setTarget (relay_router.go,
	// relay_router_virtual.go), so no re-derivation can drift from dispatch.
	activeByManagedTarget := map[string]int{}
	activeByEndpointName := map[string]int{}
	activeByVirtualName := map[string]int{}
	for _, info := range proxyInfos {
		switch info.TargetKind {
		case "managed":
			activeByManagedTarget[info.Target]++
		case "endpoint":
			if name, _, ok := strings.Cut(info.Target, "/"); ok {
				activeByEndpointName[name]++
			}
		case "virtual":
			activeByVirtualName[info.Model]++
		}
	}

	var epStatuses []registry.EndpointStatus
	if deps.Registry != nil {
		epStatuses = deps.Registry.Snapshot(ctx)
	}
	pinCounts := deps.Router.AffinityPinCounts()

	instances, catalog := detailedManagedRows(deps.Managers, activeByManagedTarget)
	virtualRows := detailedVirtualRows(deps.Virtual, epStatuses, deps.Managers, activeByVirtualName, pinCounts)
	endpointRows := detailedEndpointRows(epStatuses, activeByEndpointName, now)

	budgets := []servermanager.BudgetInfo{}
	for _, mgr := range deps.Managers {
		budgets = append(budgets, mgr.Budget())
	}

	managedRunning := 0
	for _, inst := range instances {
		if !inst.Exited {
			managedRunning++
		}
	}
	endpointsOnline := 0
	for _, s := range epStatuses {
		if s.Online {
			endpointsOnline++
		}
	}

	routerAddr := ""
	var routerAddrs []string
	routerTLS := false
	routerSocket := ""
	if deps.Router != nil {
		routerAddr = deps.Router.Addr()
		routerAddrs = deps.Router.Addrs()
		routerTLS = deps.Router.TLSEnabled()
		routerSocket = deps.Router.SocketPath()
	}
	// tcp and socket are reported separately because router.sock (C9) can be
	// live with no TCP listener bound at all (relay launched this process
	// with --router-port unset): Addr()/Addrs() alone would otherwise read as
	// "router disabled" even though it is actively serving relay's model
	// broker over the socket.
	routerTCPEnabled := routerAddr != "" || len(routerAddrs) > 0

	connections := make([]map[string]any, 0, len(proxyInfos))
	for _, info := range proxyInfos {
		connections = append(connections, detailedProxyRow(info))
	}

	recentOut := make([]map[string]any, 0, len(recentReqs))
	for _, rr := range recentReqs {
		recentOut = append(recentOut, detailedRecentRequestRow(rr))
	}

	instanceRows := make([]detailedInstanceRow, len(instances))
	copy(instanceRows, instances)

	return map[string]any{
		"generatedAt":   now.UTC().Format(time.RFC3339),
		"uptimeSeconds": int64(now.Sub(deps.StartTime).Seconds()),
		"overview": map[string]any{
			"proxyConnections":    proxyAgg.ActiveCount,
			"proxyStalled":        proxyAgg.StalledCount,
			"managedInstances":    managedRunning,
			"endpointsOnline":     endpointsOnline,
			"endpointsConfigured": len(epStatuses),
			"router": map[string]any{
				"enabled": deps.Router != nil,
				"tcp":     routerTCPEnabled,
				"addr":    routerAddr,
				"addrs":   routerAddrs,
				"tls":     routerTLS,
				"socket":  routerSocket,
			},
			"throughput": map[string]any{
				"bytesInPerSec":  proxyAgg.BytesInPerSec,
				"bytesOutPerSec": proxyAgg.BytesOutPerSec,
				"totalBytesIn":   proxyAgg.TotalBytesIn,
				"totalBytesOut":  proxyAgg.TotalBytesOut,
				"totalRequests":  proxyAgg.TotalRequests,
				"windowSeconds":  proxyAgg.WindowSeconds,
			},
		},
		"connections":    connections,
		"recentRequests": recentOut,
		"models": map[string]any{
			"instances": instanceRows,
			"catalog":   catalog,
			"virtual":   virtualRows,
			"endpoints": endpointRows,
		},
		"budgets": budgets,
	}
}

// ---------------------------------------------------------------------------
// connections[] row builders — RECONCILED_SCHEMA.md §3's unified shape.
// Every array field is initialized, never left nil, and fields that don't
// apply to a given kind are simply omitted (not sent as null) per §3.
// ---------------------------------------------------------------------------

func detailedProxyRow(info router.ProxyConnInfo) map[string]any {
	kind := "http"
	if info.Stream {
		kind = "sse"
	}
	row := map[string]any{
		"kind":           kind,
		"id":             strconv.FormatUint(info.ID, 10),
		"state":          info.State,
		"targetKind":     info.TargetKind,
		"target":         info.Target,
		"model":          info.Model,
		"attempts":       info.Attempts,
		"method":         info.Method,
		"path":           info.Path,
		"remoteAddr":     info.RemoteAddr,
		"startedAt":      info.StartedAt.UTC().Format(time.RFC3339),
		"ageSeconds":     info.AgeSeconds,
		"bytesIn":        info.BytesIn,
		"bytesOut":       info.BytesOut,
		"bytesOutPerSec": info.BytesOutPerSec,
		"streaming":      info.Stream,
		"viaAnthropic":   info.ViaAnthropic,
		"status":         info.Status,
	}
	if !info.LastByteAt.IsZero() {
		row["lastByteAt"] = info.LastByteAt.UTC().Format(time.RFC3339)
		row["sinceLastByteSeconds"] = info.SinceLastByteSeconds
	}
	return row
}

func detailedRecentRequestRow(rr router.RecentRequestInfo) map[string]any {
	return map[string]any{
		"id":         strconv.FormatUint(rr.ID, 10),
		"model":      rr.Model,
		"targetKind": rr.TargetKind,
		"target":     rr.Target,
		"attempts":   rr.Attempts,
		"status":     rr.Status,
		"stream":     rr.Stream,
		"durationMs": rr.DurationMs,
		"ttfbMs":     rr.TTFBMs,
		"bytesIn":    rr.BytesIn,
		"bytesOut":   rr.BytesOut,
		"finishedAt": rr.FinishedAt.UTC().Format(time.RFC3339),
	}
}

// ---------------------------------------------------------------------------
// Managed-server rows (models.instances / models.catalog)
// ---------------------------------------------------------------------------

// detailedInstanceRow embeds ServerInstanceInfo (already json-tagged) rather
// than copying its fields, adding only what this view needs on top.
type detailedInstanceRow struct {
	servermanager.ServerInstanceInfo
	Kind           string `json:"kind"`
	ActiveRequests int    `json:"activeRequests"`
}

// detailedCatalogRow embeds ManagedModelInfo the same way.
type detailedCatalogRow struct {
	servermanager.ManagedModelInfo
	Kind string `json:"kind"`
}

func detailedManagedRows(managers []*servermanager.ServerManager, activeByManagedTarget map[string]int) ([]detailedInstanceRow, []detailedCatalogRow) {
	instances := []detailedInstanceRow{}
	catalog := []detailedCatalogRow{}
	for _, mgr := range managers {
		kind := mgr.Profile().Kind
		for _, inst := range mgr.ListInstances() {
			instances = append(instances, detailedInstanceRow{
				ServerInstanceInfo: inst,
				Kind:               kind,
				// Matches exactly the target string routeManaged stores via
				// conn.setTarget("managed", mgr.Profile().Kind+":"+alias).
				ActiveRequests: activeByManagedTarget[kind+":"+inst.Alias],
			})
		}
		for _, entry := range mgr.ModelCatalog() {
			catalog = append(catalog, detailedCatalogRow{ManagedModelInfo: entry, Kind: kind})
		}
	}
	sort.Slice(instances, func(i, j int) bool {
		if instances[i].Kind != instances[j].Kind {
			return instances[i].Kind < instances[j].Kind
		}
		return instances[i].Alias < instances[j].Alias
	})
	sort.Slice(catalog, func(i, j int) bool {
		if catalog[i].Kind != catalog[j].Kind {
			return catalog[i].Kind < catalog[j].Kind
		}
		return catalog[i].Alias < catalog[j].Alias
	})
	return instances, catalog
}

// ---------------------------------------------------------------------------
// Virtual-model rows (models.virtual)
// ---------------------------------------------------------------------------

func detailedVirtualRows(virtual *config.VirtualLLMConfig, epStatuses []registry.EndpointStatus, managers []*servermanager.ServerManager, activeByVirtualName map[string]int, pinCounts map[string]map[string]int) []map[string]any {
	rows := []map[string]any{}
	if virtual == nil {
		return rows
	}
	for i := range virtual.Models {
		v := &virtual.Models[i]
		candidates, freshCount := router.CandidatesForVirtual(v, epStatuses, managers)

		status := servermanager.ModelStatusUnloaded
		if freshCount > 0 {
			status = servermanager.ModelStatusLoaded
		}

		candRows := make([]map[string]any, 0, len(candidates))
		for order, c := range candidates {
			kind := "endpoint"
			if c.Manager() != nil {
				kind = "alias"
			}
			identity := c.Identity()
			candRows = append(candRows, map[string]any{
				"order":               order,
				"kind":                kind,
				"identity":            identity,
				"label":               c.Label(),
				"reachable":           order < freshCount,
				"pinnedConversations": pinCounts[v.Name][identity],
			})
		}

		rows = append(rows, map[string]any{
			"name":                v.Name,
			"status":              status,
			"reachableCandidates": freshCount,
			"activeRequests":      activeByVirtualName[v.Name],
			"candidates":          candRows,
		})
	}
	return rows
}

// ---------------------------------------------------------------------------
// Endpoint rows (models.endpoints)
// ---------------------------------------------------------------------------

// detailedEndpointRows builds each row explicitly from EndpointStatus's
// individual fields rather than marshaling EndpointStatus (or its embedded
// config.OpenAIEndpoint) directly. This is a hard rule, not a style choice:
// config.OpenAIEndpoint carries APIKey (and, for a TLS-pinned endpoint, CAFile/
// PinSHA256) — a direct marshal would leak the credential into a page every
// browser tab with access to this dashboard can read. Covered by
// TestSec_DetailedStatus_NeverLeaksEndpointSecrets in
// security_regression_test.go. Field name is "error", matching
// ManagedModelInfo.Error's naming convention (RECONCILED_SCHEMA.md §6) — not
// "lastError".
func detailedEndpointRows(statuses []registry.EndpointStatus, activeByEndpointName map[string]int, now time.Time) []map[string]any {
	rows := make([]map[string]any, 0, len(statuses))
	for _, s := range statuses {
		models := make([]string, 0, len(s.Models))
		for _, m := range s.Models {
			models = append(models, m.ID)
		}
		age := int(now.Sub(s.LastChecked).Seconds())
		if age < 0 {
			age = 0
		}
		rows = append(rows, map[string]any{
			"name":           s.Endpoint.Name,
			"baseURL":        s.Endpoint.BaseURL,
			"group":          s.Endpoint.Group,
			"online":         s.Online,
			"lastChecked":    s.LastChecked.UTC().Format(time.RFC3339),
			"ageSeconds":     age,
			"error":          s.Err,
			"modelCount":     len(s.Models),
			"models":         models,
			"activeRequests": activeByEndpointName[s.Endpoint.Name],
		})
	}
	return rows
}
