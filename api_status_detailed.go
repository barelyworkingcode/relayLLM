package main

// GET /api/status/detailed and GET /status — the diagnostic status
// dashboard's JSON feed and its embedded HTML shell. Deliberately separate
// from api_status.go's GET /api/status: that payload is relay's Service
// Inspector contract and must stay byte-identical (see CLAUDE.md's
// Relay-router section and api_status.go's own doc comment) — this is a
// different, additive endpoint for a different consumer, so it is free to
// carry a much richer shape.
//
// buildDetailedStatus takes each subsystem's own snapshot independently and
// in no particular order (ProxyMetrics.Snapshot, WSHub.SnapshotConnections,
// ProxyRegistry.Snapshot, ServerManager.ListInstances/ModelCatalog/Budget,
// TerminalManager.ListSummary, and a direct read of SessionManager's session
// map under its own lock) — it never holds two subsystems' locks at once,
// since every call here is a "snapshot()-shaped, returns a copy" call.

import (
	"context"
	"embed"
	"net/http"
	clk "relayllm/internal/clock"
	"relayllm/internal/config"
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
// Every field except Sessions is nil-safe to omit: a nil Router yields
// overview.router.enabled == false and an empty proxy-connections/virtual
// section; a nil Registry yields an empty endpoints array; nil Managers,
// Virtual, and Terminals behave the same way. Clock nil -> DefaultClock.
type DetailedStatusDeps struct {
	Sessions  *SessionManager
	Terminals *TerminalManager
	WSHub     *WSHub
	Managers  []*ServerManager // dispatch priority order: llama, then mlx
	Registry  *ProxyRegistry   // may be nil
	Virtual   *config.VirtualLLMConfig
	Router    *RelayRouter // may be nil (--router-port unset)
	StartTime time.Time
	Clock     clk.Clock
}

// RegisterDetailedStatusRoutes wires GET /api/status/detailed and the
// embedded GET /status dashboard (plus its /status/*.css /status/*.js
// assets) onto the main mux. Deliberately NOT the relay-router's TCP
// listener (see J8 in backend.md's design notes): that listener is
// unauthenticated by design (a local OpenAI-compatible endpoint), and this
// page exposes session directories, model config, and endpoint names. The
// main mux sits behind bearerAuth, so /status reached from a browser goes
// through relay's front door, which supplies the token.
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

	wsConns := []WSConnInfo{}
	if deps.WSHub != nil {
		wsConns = deps.WSHub.SnapshotConnections()
	}
	viewersBySession := make(map[string]int)
	for _, wc := range wsConns {
		for _, sid := range wc.Sessions {
			viewersBySession[sid]++
		}
	}

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

	var epStatuses []EndpointStatus
	if deps.Registry != nil {
		epStatuses = deps.Registry.Snapshot(ctx)
	}
	pinCounts := deps.Router.AffinityPinCounts()

	instances, catalog := detailedManagedRows(deps.Managers, activeByManagedTarget)
	virtualRows := detailedVirtualRows(deps.Virtual, epStatuses, deps.Managers, activeByVirtualName, pinCounts)
	endpointRows := detailedEndpointRows(epStatuses, activeByEndpointName, now)

	budgets := []BudgetInfo{}
	for _, mgr := range deps.Managers {
		budgets = append(budgets, mgr.Budget())
	}

	terminals := []TerminalSummary{}
	if deps.Terminals != nil {
		terminals = deps.Terminals.ListSummary()
	}

	chatRows, sessionsProcessing := detailedSessionRows(deps.Sessions, viewersBySession, now)
	totalSessions := 0
	if deps.Sessions != nil {
		totalSessions = len(deps.Sessions.ListSessions())
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
	if deps.Router != nil {
		routerAddr = deps.Router.Addr()
		routerAddrs = deps.Router.Addrs()
		routerTLS = deps.Router.TLSEnabled()
	}

	connections := make([]map[string]any, 0, len(proxyInfos)+len(wsConns)+len(chatRows))
	for _, info := range proxyInfos {
		connections = append(connections, detailedProxyRow(info))
	}
	for _, wc := range wsConns {
		connections = append(connections, detailedWSRow(wc))
	}
	connections = append(connections, chatRows...)

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
			"sessions":             totalSessions,
			"sessionsProcessing":   sessionsProcessing,
			"terminals":            len(terminals),
			"websocketConnections": len(wsConns),
			"proxyConnections":     proxyAgg.ActiveCount,
			"proxyStalled":         proxyAgg.StalledCount,
			"managedInstances":     managedRunning,
			"endpointsOnline":      endpointsOnline,
			"endpointsConfigured":  len(epStatuses),
			"router": map[string]any{
				"enabled": deps.Router != nil,
				"addr":    routerAddr,
				"addrs":   routerAddrs,
				"tls":     routerTLS,
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
		"budgets":   budgets,
		"terminals": terminals,
	}
}

// ---------------------------------------------------------------------------
// connections[] row builders — RECONCILED_SCHEMA.md §3's unified shape.
// Every array field is initialized, never left nil, and fields that don't
// apply to a given kind are simply omitted (not sent as null) per §3.
// ---------------------------------------------------------------------------

func detailedProxyRow(info ProxyConnInfo) map[string]any {
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

func detailedWSRow(info WSConnInfo) map[string]any {
	sessions := info.Sessions
	if sessions == nil {
		sessions = []string{}
	}
	terminals := info.Terminals
	if terminals == nil {
		terminals = []string{}
	}
	return map[string]any{
		"kind":                 "ws",
		"id":                   strconv.FormatUint(info.ID, 10),
		"state":                info.State,
		"remoteAddr":           info.RemoteAddr,
		"startedAt":            info.ConnectedAt,
		"lastByteAt":           info.LastActivityAt,
		"ageSeconds":           info.AgeSeconds,
		"sinceLastByteSeconds": info.IdleSeconds,
		"bytesIn":              info.BytesIn,
		"bytesOut":             info.BytesOut,
		"messagesIn":           info.MessagesIn,
		"messagesOut":          info.MessagesOut,
		"sessions":             sessions,
		"terminals":            terminals,
	}
}

func detailedRecentRequestRow(rr RecentRequestInfo) map[string]any {
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
// Sessions ("chat" rows). Reads SessionManager's live session map directly
// (same package) rather than through ListSessions(), which returns a
// display-oriented map missing providerType/stats/processing — the fields
// this dashboard needs. Locking mirrors ListSessions()'s own convention
// exactly: sessions.mu.RLock for the map, then each session's own mu for its
// mutable fields, never both at once.
// ---------------------------------------------------------------------------

func detailedSessionRows(sessions *SessionManager, viewersBySession map[string]int, now time.Time) (rows []map[string]any, processingCount int) {
	if sessions == nil {
		return []map[string]any{}, 0
	}

	sessions.mu.RLock()
	list := make([]*Session, 0, len(sessions.sessions))
	for _, s := range sessions.sessions {
		if s.Headless {
			continue
		}
		list = append(list, s)
	}
	sessions.mu.RUnlock()

	rows = make([]map[string]any, 0, len(list))
	for _, s := range list {
		provider := s.Provider()
		processing := s.IsProcessing()

		s.Lock()
		id := s.ID
		name := s.Name
		projectID := s.ProjectID
		model := s.Model
		providerType := s.ProviderType
		directory := s.Directory
		createdAt := s.CreatedAt
		messageCount := len(s.Messages)
		lastMsgAt := lastMessageAt(s.Messages)
		stats := s.Stats
		s.Unlock()

		state := connStateIdle
		if processing {
			state = connStateActive
			processingCount++
		}

		row := map[string]any{
			"kind":         "chat",
			"id":           id,
			"state":        state,
			"name":         name,
			"projectId":    projectID,
			"model":        model,
			"providerType": providerType,
			"directory":    directory,
			"active":       provider != nil && provider.Alive(),
			"startedAt":    createdAt,
			"ageSeconds":   secondsSinceRFC3339(createdAt, now),
			"messageCount": messageCount,
			"viewers":      viewersBySession[id],
			"stats":        stats,
		}
		if lastMsgAt != "" {
			row["lastByteAt"] = lastMsgAt
			row["sinceLastByteSeconds"] = secondsSinceRFC3339(lastMsgAt, now)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["id"].(string) < rows[j]["id"].(string) })
	return rows, processingCount
}

// secondsSinceRFC3339 parses an RFC3339 timestamp (the format every
// timestamp in this codebase is stored in) and returns whole seconds elapsed
// since it, floored at 0. An unparseable or empty timestamp reads as 0
// rather than propagating an error into a diagnostic page that must never
// itself fail to render.
func secondsSinceRFC3339(s string, now time.Time) int {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	d := now.Sub(t)
	if d < 0 {
		return 0
	}
	return int(d.Seconds())
}

// ---------------------------------------------------------------------------
// Managed-server rows (models.instances / models.catalog)
// ---------------------------------------------------------------------------

// detailedInstanceRow embeds ServerInstanceInfo (already json-tagged) rather
// than copying its fields, adding only what this view needs on top.
type detailedInstanceRow struct {
	ServerInstanceInfo
	Kind           string `json:"kind"`
	ActiveRequests int    `json:"activeRequests"`
}

// detailedCatalogRow embeds ManagedModelInfo the same way.
type detailedCatalogRow struct {
	ManagedModelInfo
	Kind string `json:"kind"`
}

func detailedManagedRows(managers []*ServerManager, activeByManagedTarget map[string]int) ([]detailedInstanceRow, []detailedCatalogRow) {
	instances := []detailedInstanceRow{}
	catalog := []detailedCatalogRow{}
	for _, mgr := range managers {
		kind := mgr.profile.Kind
		for _, inst := range mgr.ListInstances() {
			instances = append(instances, detailedInstanceRow{
				ServerInstanceInfo: inst,
				Kind:               kind,
				// Matches exactly the target string routeManaged stores via
				// conn.setTarget("managed", mgr.profile.Kind+":"+alias).
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

func detailedVirtualRows(virtual *config.VirtualLLMConfig, epStatuses []EndpointStatus, managers []*ServerManager, activeByVirtualName map[string]int, pinCounts map[string]map[string]int) []map[string]any {
	rows := []map[string]any{}
	if virtual == nil {
		return rows
	}
	for i := range virtual.Models {
		v := &virtual.Models[i]
		candidates, freshCount := candidatesForVirtual(v, epStatuses, managers)

		status := ModelStatusUnloaded
		if freshCount > 0 {
			status = ModelStatusLoaded
		}

		candRows := make([]map[string]any, 0, len(candidates))
		for order, c := range candidates {
			kind := "endpoint"
			if c.manager != nil {
				kind = "alias"
			}
			identity := c.identity()
			candRows = append(candRows, map[string]any{
				"order":               order,
				"kind":                kind,
				"identity":            identity,
				"label":               c.label(),
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
func detailedEndpointRows(statuses []EndpointStatus, activeByEndpointName map[string]int, now time.Time) []map[string]any {
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
