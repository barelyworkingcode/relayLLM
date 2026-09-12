package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"relayllm/internal/config"
	"relayllm/internal/permission"
	"relayllm/internal/registry"
	"relayllm/internal/session"
)

// Security regression suite. One file = one audit surface. Every test here
// corresponds to a specific attack-shape that the production code already
// prevents; removing the underlying guard should flip the test to failing.
//
// Naming convention (borrowed from ../relay/security_regression_test.go): each
// test is `TestSec_<surface>_<expected-behavior>` so a future audit can scan
// one file and answer "is this covered?" without reading the bodies.
//
// Several guards here intentionally duplicate an assertion already made in a
// mechanical test (e.g. headless flags in provider_claude_spawn_test.go). The
// duplication is the point: a refactor that quietly relaxes the mechanical test
// still has to get past the security file, which reads as a checklist of
// invariants that must never regress.

// ---------------------------------------------------------------------------
// HTTP bearer auth boundary (auth.go)
// ---------------------------------------------------------------------------

const secTestToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// secAuthResponse drives a request through bearerAuth-wrapped handler and
// returns the resulting status code. The inner handler writes 200, so any
// status other than 200 means the request was rejected before reaching it.
func secAuthResponse(t *testing.T, configuredToken, authHeader string) int {
	t.Helper()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	BearerAuth(configuredToken, inner).ServeHTTP(rec, req)
	return rec.Code
}

func TestSec_BearerAuth_RejectsMissingHeader(t *testing.T) {
	if code := secAuthResponse(t, secTestToken, ""); code != http.StatusUnauthorized {
		t.Errorf("missing Authorization header: status %d; want 401", code)
	}
}

func TestSec_BearerAuth_RejectsWrongToken(t *testing.T) {
	if code := secAuthResponse(t, secTestToken, "Bearer not-the-token"); code != http.StatusUnauthorized {
		t.Errorf("wrong token: status %d; want 401", code)
	}
}

// One byte different — guards that the comparison is a real equality check, not
// a prefix/length heuristic.
func TestSec_BearerAuth_RejectsOneByteOffToken(t *testing.T) {
	off := secTestToken[:len(secTestToken)-1] + "0" // last 'f' -> '0'
	if off == secTestToken {
		t.Fatal("test setup: mutated token equals original")
	}
	if code := secAuthResponse(t, secTestToken, "Bearer "+off); code != http.StatusUnauthorized {
		t.Errorf("one-byte-off token: status %d; want 401", code)
	}
}

func TestSec_BearerAuth_RejectsEmptyBearerValue(t *testing.T) {
	if code := secAuthResponse(t, secTestToken, "Bearer "); code != http.StatusUnauthorized {
		t.Errorf("empty bearer value: status %d; want 401", code)
	}
}

// Wrong scheme (e.g. Basic) must not satisfy the Bearer requirement.
func TestSec_BearerAuth_RejectsNonBearerScheme(t *testing.T) {
	if code := secAuthResponse(t, secTestToken, "Basic "+secTestToken); code != http.StatusUnauthorized {
		t.Errorf("Basic scheme: status %d; want 401", code)
	}
}

func TestSec_BearerAuth_AcceptsCorrectToken(t *testing.T) {
	if code := secAuthResponse(t, secTestToken, "Bearer "+secTestToken); code != http.StatusOK {
		t.Errorf("correct token: status %d; want 200", code)
	}
}

// Empty configured token is the documented dev-mode pass-through (main.go warns
// loudly). This test pins that behavior so it can only ever change deliberately.
func TestSec_BearerAuth_EmptyConfiguredTokenIsPassThrough(t *testing.T) {
	if code := secAuthResponse(t, "", ""); code != http.StatusOK {
		t.Errorf("empty configured token should pass through: status %d; want 200", code)
	}
}

// bearerAuth no longer accepts a cookie or ?token= query parameter as a
// credential carrier — that browser-convenience path existed only for the
// --http-port TCP front, which now serves anonymously and never wraps
// requests in bearerAuth at all (see auth.go's doc comment). A configured,
// correctly-valued cookie or query param must therefore be inert on the one
// front that still uses bearerAuth (the Unix socket): neither one should be
// able to substitute for the Authorization header a regression could
// otherwise silently reopen.
func TestSec_BearerAuth_CookieAndQueryParamAreNoLongerCredentials(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/status?token="+secTestToken, nil)
	req.AddCookie(&http.Cookie{Name: "relayllm_auth", Value: secTestToken})
	rec := httptest.NewRecorder()
	BearerAuth(secTestToken, inner).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("valid cookie + valid ?token=, no Authorization header: status %d; want 401", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Auto-provisioned token entropy (auth.go)
// ---------------------------------------------------------------------------

func TestSec_GeneratedBearerToken_Is256BitHexAndUnique(t *testing.T) {
	a := GenerateBearerToken()
	b := GenerateBearerToken()
	if len(a) != 64 { // 32 bytes -> 64 hex chars
		t.Errorf("token length = %d; want 64 hex chars (256-bit)", len(a))
	}
	if a == b {
		t.Error("two generated tokens are identical; entropy source is broken")
	}
}

// Claude spawn/env/host-exec security tests moved to
// internal/provider/claude_security_test.go when the Claude provider moved
// to its own package. The host-terminal-exec security test moved to
// internal/terminal/terminal_security_test.go when terminal_session.go
// moved to its own package.

// ---------------------------------------------------------------------------
// GET /api/status/detailed must never leak an endpoint's credentials
// (api_status_detailed.go). EndpointStatus embeds the full config.OpenAIEndpoint,
// including APIKey (and, for a TLS-pinned endpoint, CAFile/PinSHA256) — the
// endpoint rows this handler builds must construct each field explicitly
// rather than ever marshaling EndpointStatus/config.OpenAIEndpoint directly.
// ---------------------------------------------------------------------------

func TestSec_DetailedStatus_NeverLeaksEndpointSecrets(t *testing.T) {
	const secretKey = "sk-super-secret-do-not-leak-1234567890"
	ep := config.OpenAIEndpoint{Name: "leaky", BaseURL: "http://127.0.0.1:1/v1", APIKey: secretKey}
	reg := registry.NewProxyRegistry(&config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{ep}})
	reg.SetStatusForTest(ep, true, registry.UpstreamModel{ID: "m"})

	sessions := session.NewSessionManager(session.NewSessionStore(t.TempDir()), permission.NewPermissionManager())
	deps := DetailedStatusDeps{
		Sessions:  sessions,
		Registry:  reg,
		StartTime: time.Now(),
	}
	got := buildDetailedStatus(context.Background(), deps)

	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal detailed status: %v", err)
	}
	if strings.Contains(string(data), secretKey) {
		t.Errorf("GET /api/status/detailed leaked the endpoint's APIKey into the response body")
	}
}
