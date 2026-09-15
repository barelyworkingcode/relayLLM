package api

// Hermetic coverage for api_status_detailed.go: nil-dependency safety, the
// top-level JSON shape, virtual-model candidate reachability/pin reporting,
// and the embedded /status page's auth + serving behavior.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"relayllm/internal/config"
	"relayllm/internal/peertoken"
	"relayllm/internal/permission"
	"relayllm/internal/registry"
	"relayllm/internal/router"
	"relayllm/internal/session"
	"relayllm/internal/terminal"
	"strings"
	"testing"
	"time"
)

func TestDetailedStatus_NilRouterAndRegistry(t *testing.T) {
	sessions := session.NewSessionManager(session.NewSessionStore(t.TempDir()), permission.NewPermissionManager())

	deps := DetailedStatusDeps{
		Sessions:  sessions,
		StartTime: time.Now(),
	}
	got := buildDetailedStatus(context.Background(), deps)

	overview, ok := got["overview"].(map[string]any)
	if !ok {
		t.Fatalf("overview missing or wrong type: %+v", got["overview"])
	}
	router, ok := overview["router"].(map[string]any)
	if !ok || router["enabled"] != false {
		t.Errorf("overview.router = %+v, want enabled:false", overview["router"])
	}

	// Every array must be [] not null — a null here breaks a JS .map() on
	// the real page.
	for _, key := range []string{"connections", "recentRequests", "budgets", "terminals"} {
		v, ok := got[key]
		if !ok {
			t.Errorf("missing top-level key %q", key)
			continue
		}
		assertJSONArray(t, key, v)
	}
	models, ok := got["models"].(map[string]any)
	if !ok {
		t.Fatalf("models missing or wrong type: %+v", got["models"])
	}
	for _, key := range []string{"instances", "catalog", "virtual", "endpoints"} {
		v, ok := models[key]
		if !ok {
			t.Errorf("models missing key %q", key)
			continue
		}
		assertJSONArray(t, "models."+key, v)
	}
}

// assertJSONArray marshals v and confirms it round-trips as a JSON array
// (never null) — the actual contract the frontend depends on, checked the
// same way a JS client would see it rather than via Go's own nil slice
// semantics (which a %v Errorf could get wrong for a typed nil slice).
func assertJSONArray(t *testing.T, name string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" {
		t.Errorf("%s marshaled to null, want []", name)
	}
	if !strings.HasPrefix(trimmed, "[") {
		t.Errorf("%s marshaled to %s, want a JSON array", name, trimmed)
	}
}

func TestDetailedStatus_Shape(t *testing.T) {
	sessions := session.NewSessionManager(session.NewSessionStore(t.TempDir()), permission.NewPermissionManager())
	perms := permission.NewPermissionManager()
	terminals := terminal.NewTerminalManager(terminal.NewTemplateStore(t.TempDir()), t.TempDir())
	wsHub := NewWSHub(sessions, perms, terminals)

	deps := DetailedStatusDeps{
		Sessions:  sessions,
		Terminals: terminals,
		WSHub:     wsHub,
		StartTime: time.Now(),
	}
	got := buildDetailedStatus(context.Background(), deps)

	for _, key := range []string{"generatedAt", "uptimeSeconds", "overview", "connections", "recentRequests", "models", "budgets", "terminals"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing top-level key %q in %+v", key, got)
		}
	}
	if _, err := time.Parse(time.RFC3339, got["generatedAt"].(string)); err != nil {
		t.Errorf("generatedAt = %v, want RFC3339: %v", got["generatedAt"], err)
	}

	// Every connections[] row (of whatever kind ends up present) must carry
	// the "kind" discriminator so a client can dispatch on it — a rename
	// here would silently break the frontend's reconciliation, hence a
	// direct shape assertion rather than trusting the Go struct alone.
	connections, ok := got["connections"].([]map[string]any)
	if !ok {
		t.Fatalf("connections wrong type: %T", got["connections"])
	}
	for _, row := range connections {
		if _, ok := row["kind"]; !ok {
			t.Errorf("connections[] row missing kind discriminator: %+v", row)
		}
	}
}

// TestDetailedStatus_SocketOnlyRouter pins the C9 should-fix: a router.sock-only
// deployment (relay launched relayLLM with --router-port unset) must not
// report overview.router.enabled == true with no other signal that TCP isn't
// actually bound — a dashboard reader (or a future status.js feature) needs
// tcp/socket to tell "live over the socket only" apart from "live over TCP".
func TestDetailedStatus_SocketOnlyRouter(t *testing.T) {
	// BuildRelayRouter, not NewRelayRouter(":0", ...) directly: production
	// (app.go) always builds with an empty addr and lets MaybeServeTCP/Listen
	// fill it in only if TCP actually binds — using ":0" here would report a
	// non-empty Addr() even though nothing was ever bound, which is exactly
	// the bug this test exists to catch.
	rtr := router.BuildRelayRouter(nil, nil, nil, nil, "", "")
	dir, err := os.MkdirTemp("/tmp", "statustest")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "r.sock")
	if err := rtr.ListenSocket(sockPath, func() (peertoken.Process, bool) { return peertoken.Process{}, false }); err != nil {
		t.Fatalf("ListenSocket: %v", err)
	}
	t.Cleanup(func() { _ = rtr.Close() })

	sessions := session.NewSessionManager(session.NewSessionStore(t.TempDir()), permission.NewPermissionManager())
	deps := DetailedStatusDeps{
		Sessions:  sessions,
		Router:    rtr,
		StartTime: time.Now(),
	}
	got := buildDetailedStatus(context.Background(), deps)

	overview := got["overview"].(map[string]any)
	routerJSON, ok := overview["router"].(map[string]any)
	if !ok {
		t.Fatalf("overview.router missing or wrong type: %+v", overview["router"])
	}
	if routerJSON["enabled"] != true {
		t.Errorf("enabled = %v, want true (router.sock is live)", routerJSON["enabled"])
	}
	if routerJSON["tcp"] != false {
		t.Errorf("tcp = %v, want false (no --router-port bound)", routerJSON["tcp"])
	}
	if routerJSON["addr"] != "" {
		t.Errorf("addr = %v, want \"\" with no TCP listener", routerJSON["addr"])
	}
	if routerJSON["socket"] != sockPath {
		t.Errorf("socket = %v, want %q", routerJSON["socket"], sockPath)
	}
}

// A configured virtual model with one online and one offline endpoint must
// report `reachable` matching candidatesForVirtual's own freshCount split,
// and a recorded affinity pin must surface as pinnedConversations on the
// pinned candidate — never on the other one.
func TestDetailedStatus_VirtualCandidateReachability(t *testing.T) {
	onlineEP := config.OpenAIEndpoint{Name: "online", BaseURL: "http://127.0.0.1:1/v1"}
	offlineEP := config.OpenAIEndpoint{Name: "offline", BaseURL: "http://127.0.0.1:1/v1"}
	reg := registry.NewProxyRegistry(&config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{onlineEP, offlineEP}})
	reg.SetStatusForTest(onlineEP, true, registry.UpstreamModel{ID: "m"})
	reg.SetStatusForTest(offlineEP, false)

	virtual := &config.VirtualLLMConfig{Models: []config.VirtualLLM{{
		Name: "vMixed",
		Targets: []config.VirtualLLMTarget{
			{Endpoint: "offline", Model: "m"},
			{Endpoint: "online", Model: "m"},
		},
	}}}

	rtr := router.NewRelayRouter(":0", nil, reg, virtual)
	// Record a pin directly on the affinity store via the test seam — this is
	// exactly what routeVirtual does on a successful response (see
	// relay_router_virtual.go), without needing a live HTTP round trip here.
	onlineIdentity := router.EndpointTargetIdentity(onlineEP, "m")
	rtr.RecordAffinityForTest("vMixed", "conv-1", onlineIdentity)

	sessions := session.NewSessionManager(session.NewSessionStore(t.TempDir()), permission.NewPermissionManager())
	deps := DetailedStatusDeps{
		Sessions:  sessions,
		Registry:  reg,
		Virtual:   virtual,
		Router:    rtr,
		StartTime: time.Now(),
	}
	got := buildDetailedStatus(context.Background(), deps)

	models := got["models"].(map[string]any)
	virtualRows := models["virtual"].([]map[string]any)
	if len(virtualRows) != 1 {
		t.Fatalf("virtual rows = %+v, want exactly 1", virtualRows)
	}
	row := virtualRows[0]
	if row["reachableCandidates"] != 1 {
		t.Errorf("reachableCandidates = %v, want 1 (only \"online\" is fresh)", row["reachableCandidates"])
	}
	candidates := row["candidates"].([]map[string]any)
	if len(candidates) != 2 {
		t.Fatalf("candidates = %+v, want 2", candidates)
	}
	var sawOnlinePinned, sawOfflineUnpinned bool
	for _, c := range candidates {
		identity, _ := c["identity"].(string)
		reachable, _ := c["reachable"].(bool)
		pinned, _ := c["pinnedConversations"].(int)
		switch identity {
		case onlineIdentity:
			if !reachable {
				t.Errorf("online candidate reachable = %v, want true", reachable)
			}
			if pinned != 1 {
				t.Errorf("online candidate pinnedConversations = %d, want 1", pinned)
			}
			sawOnlinePinned = true
		default:
			if reachable {
				t.Errorf("offline candidate %q reachable = %v, want false", identity, reachable)
			}
			if pinned != 0 {
				t.Errorf("offline candidate %q pinnedConversations = %d, want 0 (pin belongs to online only)", identity, pinned)
			}
			sawOfflineUnpinned = true
		}
	}
	if !sawOnlinePinned || !sawOfflineUnpinned {
		t.Errorf("did not see both expected candidates: online-pinned=%v offline-unpinned=%v", sawOnlinePinned, sawOfflineUnpinned)
	}
}

// ---------------------------------------------------------------------------
// GET /status — served through the full TestServer stack (bearerAuth chain).
// ---------------------------------------------------------------------------

func TestStatusPage_ServesHTML(t *testing.T) {
	srv := NewTestServer(t, nil)
	req := srv.RawRequest(http.MethodGet, "/status", nil)
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", httpResp.StatusCode)
	}
	if ct := httpResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestStatusPage_RequiresAuth(t *testing.T) {
	srv := NewTestServer(t, nil)

	for _, path := range []string{"/status", "/api/status/detailed"} {
		req, err := http.NewRequest(http.MethodGet, srv.HTTP.URL+path, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		// Deliberately no Authorization header.
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without auth = %d, want 401", path, resp.StatusCode)
		}
	}
}
