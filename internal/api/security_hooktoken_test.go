package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Security regression suite for HookScopedBearerAuth (auth.go). Same
// convention as security_test.go: one file per audit surface, unit-level
// against the middleware directly (a fake HookTokenValidator, no session
// manager or real listener needed) so the route/method exception can't
// silently widen.

const secHookTestToken = "hook-scoped-test-internal-bearer-0123456789abcdef"

// secHookValidator accepts exactly one token, bound to one session — enough
// to exercise every branch of HookScopedBearerAuth without any real
// permission.PermissionManager.
func secHookValidator(token string) (string, bool) {
	if token == "hook-token-for-sess-1" {
		return "sess-1", true
	}
	return "", false
}

func hookAuthResponse(t *testing.T, method, path, authHeader string, validate HookTokenValidator) int {
	t.Helper()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(method, path, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	HookScopedBearerAuth(secHookTestToken, validate, inner).ServeHTTP(rec, req)
	return rec.Code
}

func TestSec_HookScopedBearerAuth_MasterTokenStillWorksEverywhere(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/sessions"},
		{"POST", "/api/permission"},
		{"GET", "/ws"},
		{"POST", "/api/terminals"},
	} {
		if code := hookAuthResponse(t, tc.method, tc.path, "Bearer "+secHookTestToken, secHookValidator); code != http.StatusOK {
			t.Errorf("%s %s with master token: status %d; want 200", tc.method, tc.path, code)
		}
	}
}

func TestSec_HookScopedBearerAuth_ValidHookToken_AllowedOnItsOneRoute(t *testing.T) {
	if code := hookAuthResponse(t, "POST", "/api/permission", "Bearer hook-token-for-sess-1", secHookValidator); code != http.StatusOK {
		t.Errorf("POST /api/permission with a valid hook token: status %d; want 200", code)
	}
}

func TestSec_HookScopedBearerAuth_ValidHookToken_RejectedEverywhereElse(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/sessions"},
		{"GET", "/api/status"},
		{"POST", "/api/terminals"},
		{"DELETE", "/api/sessions/sess-1"},
		{"GET", "/ws"},
		{"GET", "/api/permission"}, // right path, wrong method
	} {
		if code := hookAuthResponse(t, tc.method, tc.path, "Bearer hook-token-for-sess-1", secHookValidator); code != http.StatusUnauthorized {
			t.Errorf("%s %s with a hook token: status %d; want 401", tc.method, tc.path, code)
		}
	}
}

func TestSec_HookScopedBearerAuth_UnknownOrRevokedToken_RejectedEvenOnPermissionRoute(t *testing.T) {
	alwaysReject := func(string) (string, bool) { return "", false }
	if code := hookAuthResponse(t, "POST", "/api/permission", "Bearer anything", alwaysReject); code != http.StatusUnauthorized {
		t.Errorf("status %d; want 401", code)
	}
}

func TestSec_HookScopedBearerAuth_NilValidatorNeverAcceptsAnything(t *testing.T) {
	if code := hookAuthResponse(t, "POST", "/api/permission", "Bearer hook-token-for-sess-1", nil); code != http.StatusUnauthorized {
		t.Errorf("status %d; want 401 (no validator wired)", code)
	}
}

func TestSec_HookScopedBearerAuth_MissingOrMalformedHeaderRejected(t *testing.T) {
	if code := hookAuthResponse(t, "POST", "/api/permission", "", secHookValidator); code != http.StatusUnauthorized {
		t.Errorf("missing header: status %d; want 401", code)
	}
	if code := hookAuthResponse(t, "POST", "/api/permission", "Basic hook-token-for-sess-1", secHookValidator); code != http.StatusUnauthorized {
		t.Errorf("wrong scheme: status %d; want 401", code)
	}
}
