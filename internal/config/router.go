package config

// RouterConfig holds relay-router behavior that doesn't belong to any one
// backend — the Anthropic compatibility routes and the credential
// passthrough mounts. Maps to settings.json's optional
// top-level "router" section.
type RouterConfig struct {
	// Anthropic enables Anthropic Messages API compatibility (/v1/messages,
	// /v1/messages/count_tokens, and an /api/ passthrough) on this same
	// listener — see relay_router_anthropic.go. Absent (nil, the default)
	// means those routes 404; behavior is otherwise unchanged.
	Anthropic *AnthropicRouterConfig `json:"anthropic,omitempty"`

	// Passthrough mounts /<name>/ routes that forward byte-for-byte, client
	// credential included, to a fixed upstream (ChatGPT's Codex backend,
	// api.openai.com). See relay_router_passthrough.go. Absent or empty mounts
	// nothing.
	Passthrough map[string]PassthroughConfig `json:"passthrough,omitempty"`
}

// ForwardsClientCredentials reports whether any route forwards the client's
// own credential upstream (router.anthropic or router.passthrough).
// StartRelayRouter uses it to start a router that has no local backends.
// Nil-safe.
func (c *RouterConfig) ForwardsClientCredentials() bool {
	return c != nil && (c.Anthropic != nil || len(c.Passthrough) > 0)
}

// AnthropicRouterConfig is settings.json's optional router.anthropic
// section. Absent entirely -> the /v1/messages routes 404 (RelayRouter.anthropic
// stays nil) — zero behavior change from before this feature existed (off by default).
type AnthropicRouterConfig struct {
	// Upstream is the real Anthropic API base URL passthrough forwards to.
	// Defaults to "https://api.anthropic.com" when empty.
	Upstream string `json:"upstream,omitempty"`

	// ModelMap redirects specific Anthropic model ids (exact, case-sensitive
	// match against the request body's top-level "model" field; keys are
	// free-form strings) to a
	// router-dispatchable target: a managed-server alias, a configured
	// virtual model name, or an "endpoint/model" id. A key absent from this
	// map (the common case, and the only case when ModelMap is empty or
	// absent) passes straight through to Upstream untouched.
	ModelMap map[string]string `json:"modelMap,omitempty"`

	// PingIntervalSeconds sets how often a "ping" SSE event is sent during a
	// redirected streaming response to keep Claude Code's byte-idle watchdog
	// from firing while a local model is still processing a long prompt.
	// Defaults to 15 when zero or negative.
	PingIntervalSeconds int `json:"pingIntervalSeconds,omitempty"`
}

// PassthroughConfig is one entry of settings.json's router.passthrough map.
// The map key is the path segment the route mounts at.
type PassthroughConfig struct {
	// Upstream is the base URL /<name>/<rest> forwards to as
	// <Upstream>/<rest>, e.g. "https://chatgpt.com/backend-api".
	Upstream string `json:"upstream"`
}
