package servermanager

import (
	"strings"
	"testing"

	"relayllm/internal/relay"
)

// Security regression suite. One file = one audit surface: childBaseEnv,
// the only exec.Command call site in this package builds a spawned
// llama-server/mlx-serve child's environment from it instead of
// os.Environ() directly. Every test here corresponds to a relay credential
// that must never reach that child; removing the underlying strip should
// flip the matching test to failing.

func envHasKey(env []string, key string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}

// childBaseEnv must strip ALL relay credential names, including any
// inherited project-token value, so nothing leaks into a spawned model
// server we don't explicitly inject a scoped token for.
func TestChildBaseEnv_StripsRelaySecrets(t *testing.T) {
	t.Setenv(relay.EnvServiceToken, "svc-secret")
	t.Setenv(relay.EnvServiceTokenLegacy, "mcp-secret")
	t.Setenv(relay.EnvFrontendToken, "frontend-secret")
	t.Setenv(relay.EnvProjectToken, "stale-project")
	t.Setenv(relay.EnvProjectTokenLegacy, "stale-legacy-project")
	t.Setenv(relay.EnvLaunchFD, "3")

	env := childBaseEnv()
	for _, k := range []string{relay.EnvServiceToken, relay.EnvServiceTokenLegacy, relay.EnvFrontendToken, relay.EnvProjectToken, relay.EnvProjectTokenLegacy, relay.EnvLaunchFD} {
		if envHasKey(env, k) {
			t.Errorf("%s must be stripped from child base env", k)
		}
	}
	if !envHasKey(env, "PATH") {
		t.Error("non-relay env (PATH) must be preserved")
	}
}

// RELAY_LLM_TOKEN authenticates every request on relayLLM's own socket —
// strictly more power than any spawned model server needs — and
// RELAY_LLM_HOOK_TOKEN is a stale name a leftover environment could still
// carry. Both must be stripped the same as the relay-owned credentials.
func TestChildBaseEnv_StripsInternalBearerAndHookToken(t *testing.T) {
	t.Setenv("RELAY_LLM_TOKEN", "internal-bearer-secret")
	t.Setenv("RELAY_LLM_HOOK_TOKEN", "stale-hook-token")

	env := childBaseEnv()
	for _, k := range []string{"RELAY_LLM_TOKEN", "RELAY_LLM_HOOK_TOKEN"} {
		if envHasKey(env, k) {
			t.Errorf("%s must be stripped from child base env", k)
		}
	}
}

// Belt and suspenders alongside the exact-prefix-match assertions above:
// this pins childBaseEnv as the thing a future debug log of a spawned
// model server's environment would actually reflect, under the exact key
// set relaySecretEnvKeys names today.
func TestSec_ChildBaseEnv_SpawnedServerEnvHasNoRelaySecrets(t *testing.T) {
	t.Setenv(relay.EnvServiceToken, "svc-secret")
	t.Setenv(relay.EnvProjectToken, "proj-secret")
	t.Setenv(relay.EnvProjectTokenLegacy, "proj-secret-legacy")

	env := childBaseEnv()
	for _, k := range []string{relay.EnvServiceToken, relay.EnvServiceTokenLegacy, relay.EnvFrontendToken, relay.EnvProjectToken, relay.EnvProjectTokenLegacy} {
		if envHasKey(env, k) {
			t.Errorf("spawned model server child env leaked %s", k)
		}
	}
}
