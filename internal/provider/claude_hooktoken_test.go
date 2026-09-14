package provider

import (
	"encoding/json"
	"testing"

	"relayllm/internal/permission"
	"relayllm/internal/types"
)

// Hermetic coverage of the per-session hook credential's lifecycle at the
// ClaudeProvider level: minted at construction, bound to the session,
// revoked on Kill(). The HTTP-layer enforcement (which routes accept it,
// the session-match check) is covered in internal/api's tests; this file
// only checks that ClaudeProvider drives permission.PermissionManager
// correctly, since that's the one piece HTTP-layer tests can't see.

func noopHandler(string, json.RawMessage) {}

func TestNewClaudeProvider_MintsTokenBoundToSession(t *testing.T) {
	perms := permission.NewPermissionManager()
	session := &types.Session{ID: "sess-mint-1"}

	p := NewClaudeProvider(session, noopHandler, "/tmp/hook.sock", perms)

	if p.hookToken == "" {
		t.Fatal("NewClaudeProvider left hookToken empty with a non-nil perms")
	}
	sessionID, ok := perms.ValidateHookToken(p.hookToken)
	if !ok || sessionID != "sess-mint-1" {
		t.Errorf("ValidateHookToken(p.hookToken) = (%q, %v); want (sess-mint-1, true)", sessionID, ok)
	}
}

func TestNewClaudeProvider_DistinctSessionsGetDistinctTokens(t *testing.T) {
	perms := permission.NewPermissionManager()
	p1 := NewClaudeProvider(&types.Session{ID: "sess-a"}, noopHandler, "/tmp/hook.sock", perms)
	p2 := NewClaudeProvider(&types.Session{ID: "sess-b"}, noopHandler, "/tmp/hook.sock", perms)

	if p1.hookToken == p2.hookToken {
		t.Fatal("two sessions' providers minted the identical hook token")
	}
}

func TestNewClaudeProvider_NilPermsLeavesTokenEmpty(t *testing.T) {
	// Mirrors host-session construction sites that may not wire perms; must
	// not panic, and buildClaudeEnv already omits an empty hookToken.
	p := NewClaudeProvider(&types.Session{ID: "sess-nil-perms"}, noopHandler, "/tmp/hook.sock", nil)
	if p.hookToken != "" {
		t.Errorf("hookToken = %q with nil perms; want empty", p.hookToken)
	}
}

func TestClaudeProvider_Kill_RevokesHookToken(t *testing.T) {
	perms := permission.NewPermissionManager()
	session := &types.Session{ID: "sess-kill-1"}
	p := NewClaudeProvider(session, noopHandler, "/tmp/hook.sock", perms)
	token := p.hookToken

	// Kill() with no live process (p.cmd is nil) still must revoke the
	// token — the revoke happens before the nil-process early return.
	p.Kill()

	if _, ok := perms.ValidateHookToken(token); ok {
		t.Error("hook token still valid for a killed session's provider")
	}
}

func TestClaudeProvider_ReplacementProviderInvalidatesPriorToken(t *testing.T) {
	// Models SessionManager.ClearSession/SendMessage's restart path: kill
	// the old provider, construct a fresh one for the same session id. The
	// old token must not keep working once the new one exists.
	perms := permission.NewPermissionManager()
	session := &types.Session{ID: "sess-restart-1"}

	first := NewClaudeProvider(session, noopHandler, "/tmp/hook.sock", perms)
	oldToken := first.hookToken
	first.Kill()

	second := NewClaudeProvider(session, noopHandler, "/tmp/hook.sock", perms)

	if _, ok := perms.ValidateHookToken(oldToken); ok {
		t.Error("prior provider's token still valid after replacement")
	}
	sessionID, ok := perms.ValidateHookToken(second.hookToken)
	if !ok || sessionID != "sess-restart-1" {
		t.Errorf("new provider's token = (%q, %v); want (sess-restart-1, true)", sessionID, ok)
	}
}
