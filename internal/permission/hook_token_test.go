package permission

import "testing"

// Hermetic coverage of the per-session permission-hook credential
// (MintHookToken/ValidateHookToken/RevokeHookToken). This is the mechanism
// that lets the PreToolUse hook authenticate on /api/permission without
// ever holding relayLLM's internal bearer — see internal/api.HookScopedBearerAuth
// and provider.NewClaudeProvider/Kill for the call sites.

func TestHookToken_MintThenValidate_ReturnsBoundSession(t *testing.T) {
	m := NewPermissionManager()
	token := m.MintHookToken("sess-1")
	if token == "" {
		t.Fatal("MintHookToken returned empty token")
	}

	sessionID, ok := m.ValidateHookToken(token)
	if !ok || sessionID != "sess-1" {
		t.Errorf("ValidateHookToken(mintedToken) = (%q, %v); want (sess-1, true)", sessionID, ok)
	}
}

func TestHookToken_WrongOrEmptyTokenNeverValidates(t *testing.T) {
	m := NewPermissionManager()
	m.MintHookToken("sess-1")

	if _, ok := m.ValidateHookToken("guessed-or-stale-token"); ok {
		t.Error("an unminted token validated")
	}
	if _, ok := m.ValidateHookToken(""); ok {
		t.Error("empty token validated")
	}
}

func TestHookToken_TwoSessionsGetDistinctTokens(t *testing.T) {
	m := NewPermissionManager()
	tokenA := m.MintHookToken("sess-a")
	tokenB := m.MintHookToken("sess-b")

	if tokenA == tokenB {
		t.Fatal("two sessions minted the identical token")
	}
	if sid, ok := m.ValidateHookToken(tokenA); !ok || sid != "sess-a" {
		t.Errorf("tokenA validated as (%q, %v); want (sess-a, true)", sid, ok)
	}
	if sid, ok := m.ValidateHookToken(tokenB); !ok || sid != "sess-b" {
		t.Errorf("tokenB validated as (%q, %v); want (sess-b, true)", sid, ok)
	}
}

func TestHookToken_RevokeInvalidatesIt(t *testing.T) {
	m := NewPermissionManager()
	token := m.MintHookToken("sess-1")

	m.RevokeHookToken("sess-1")

	if _, ok := m.ValidateHookToken(token); ok {
		t.Error("token still validates after RevokeHookToken")
	}
}

func TestHookToken_RevokeUnknownSessionIsNoop(t *testing.T) {
	m := NewPermissionManager()
	// Must not panic when the session never had a token (e.g. a host
	// session, or Kill() called twice).
	m.RevokeHookToken("never-minted")
}

func TestHookToken_ReMintReplacesPreviousToken(t *testing.T) {
	m := NewPermissionManager()
	first := m.MintHookToken("sess-1")
	second := m.MintHookToken("sess-1")

	if first == second {
		t.Fatal("re-minting produced the same token")
	}
	if _, ok := m.ValidateHookToken(first); ok {
		t.Error("previous token still valid after re-mint — a session must have at most one live token")
	}
	if sid, ok := m.ValidateHookToken(second); !ok || sid != "sess-1" {
		t.Errorf("new token validated as (%q, %v); want (sess-1, true)", sid, ok)
	}
}
