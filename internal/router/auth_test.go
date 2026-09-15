package router

// Coverage for C10's request-time gate on the standalone TCP router:
// EnableStandaloneRouterKeys + routerKeyAuthMiddleware. keys_test.go covers
// the underlying RouterKeyStore (load/reload/permissions); router_socket_test.go's
// launched-mode admission is unaffected by any of this (see also
// internal/app's TestLaunchedMode_RouterSocketUnaffectedByRouterKeys, which
// pins that at the app.go wiring layer this package doesn't own).

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"relayllm/internal/config"
)

func TestStandaloneRouterKeys_NoFile_EveryRouteIncludingHealth401s(t *testing.T) {
	dir := t.TempDir()
	r := NewRelayRouter(":0", nil, nil, nil)
	r.EnableStandaloneRouterKeys(RouterKeysPath(dir))
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	for _, path := range []string{"/health", "/v1/models", "/"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s status = %d, want 401 (no router_keys.json exists)", path, resp.StatusCode)
		}
	}

	// Even a plausible-looking credential must not help — there is nothing
	// to compare it against.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/health", nil)
	req.Header.Set("Authorization", "Bearer rrk_"+strings.Repeat("a", 64))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status with a credential but no keys file = %d, want 401", resp.StatusCode)
	}
}

func TestStandaloneRouterKeys_BothAuthorizationBearerAndXApiKeyAccepted(t *testing.T) {
	dir := t.TempDir()
	plaintext, err := AddRouterKey(dir, "hermes")
	if err != nil {
		t.Fatalf("AddRouterKey: %v", err)
	}
	r := NewRelayRouter(":0", nil, nil, nil)
	r.EnableStandaloneRouterKeys(RouterKeysPath(dir))
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	t.Run("Authorization Bearer", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/health", nil)
		req.Header.Set("Authorization", "Bearer "+plaintext)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("x-api-key", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/health", nil)
		req.Header.Set("X-Api-Key", plaintext)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("wrong key rejected on both headers", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/health", nil)
		req.Header.Set("X-Api-Key", "rrk_"+strings.Repeat("f", 64))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 for a wrong key", resp.StatusCode)
		}
	})
}

// TestStandaloneRouterKeys_EnvVarCredentialIgnored proves the middleware
// consults only the request's own headers against the file-backed store —
// never an environment variable, no matter how plausibly named. C10 is
// explicit that the file is the only source of truth.
func TestStandaloneRouterKeys_EnvVarCredentialIgnored(t *testing.T) {
	dir := t.TempDir()
	if _, err := AddRouterKey(dir, "hermes"); err != nil {
		t.Fatalf("AddRouterKey: %v", err)
	}

	const envValue = "rrk_" + "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0"
	t.Setenv("ROUTER_KEY", envValue)
	t.Setenv("RELAY_ROUTER_KEY", envValue)
	t.Setenv("RELAY_LLM_ROUTER_KEY", envValue)

	r := NewRelayRouter(":0", nil, nil, nil)
	r.EnableStandaloneRouterKeys(RouterKeysPath(dir))
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/health", nil)
	req.Header.Set("Authorization", "Bearer "+os.Getenv("ROUTER_KEY"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a credential sourced from an env var authenticated (status %d); only router_keys.json may ever grant access", resp.StatusCode)
	}
}

// TestStandaloneRouterKeys_PassthroughRequiresDedicatedHeaderAndStripsIt pins
// the passthrough carve-out: Authorization/x-api-key are what
// router_passthrough.go forwards byte-for-byte to the real upstream, so
// neither may double as the local router-key credential on those routes —
// only X-Relay-Router-Key does, and it must never reach the upstream.
func TestStandaloneRouterKeys_PassthroughRequiresDedicatedHeaderAndStripsIt(t *testing.T) {
	dir := t.TempDir()
	plaintext, err := AddRouterKey(dir, "hermes")
	if err != nil {
		t.Fatalf("AddRouterKey: %v", err)
	}

	var gotAuth string
	var sawRouterKeyHeader bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, sawRouterKeyHeader = r.Header["X-Relay-Router-Key"]
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	r := NewRelayRouter(":0", nil, nil, nil)
	r.setPassthrough(map[string]config.PassthroughConfig{"chatgpt": {Upstream: upstream.URL}})
	r.EnableStandaloneRouterKeys(RouterKeysPath(dir))
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	// The router key on Authorization does NOT authenticate a passthrough
	// route — it would otherwise be indistinguishable from (and would leak
	// alongside) the client's real upstream credential.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/chatgpt/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Authorization alone on a passthrough route = %d, want 401", resp.StatusCode)
	}

	// x-api-key: same refusal.
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/chatgpt/v1/models", nil)
	req2.Header.Set("X-Api-Key", plaintext)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("x-api-key alone on a passthrough route = %d, want 401", resp2.StatusCode)
	}

	// The dedicated header authenticates, the client's real upstream
	// credential rides Authorization untouched, and X-Relay-Router-Key never
	// reaches the upstream.
	req3, _ := http.NewRequest(http.MethodGet, srv.URL+"/chatgpt/v1/models", nil)
	req3.Header.Set("X-Relay-Router-Key", plaintext)
	req3.Header.Set("Authorization", "Bearer real-upstream-oauth-token")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with X-Relay-Router-Key set", resp3.StatusCode)
	}
	if sawRouterKeyHeader {
		t.Error("upstream received X-Relay-Router-Key; it must be stripped before forwarding")
	}
	if gotAuth != "Bearer real-upstream-oauth-token" {
		t.Errorf("upstream Authorization = %q, want the client's real credential forwarded untouched", gotAuth)
	}
}

// TestStandaloneRouterKeys_WrongRouterKeyOnPassthroughRefused confirms the
// dedicated header is still checked against the real store, not merely
// "present".
func TestStandaloneRouterKeys_WrongRouterKeyOnPassthroughRefused(t *testing.T) {
	dir := t.TempDir()
	if _, err := AddRouterKey(dir, "hermes"); err != nil {
		t.Fatalf("AddRouterKey: %v", err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	r := NewRelayRouter(":0", nil, nil, nil)
	r.setPassthrough(map[string]config.PassthroughConfig{"chatgpt": {Upstream: upstream.URL}})
	r.EnableStandaloneRouterKeys(RouterKeysPath(dir))
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/chatgpt/v1/models", nil)
	req.Header.Set("X-Relay-Router-Key", "rrk_"+strings.Repeat("9", 64))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a wrong X-Relay-Router-Key", resp.StatusCode)
	}
}
