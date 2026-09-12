package config

// RouterConfig holds relay-router behavior that doesn't belong to any one
// backend — the reasoning-effort rewrite table and its sibling
// chat_template_kwargs merge table. Maps to settings.json's optional
// top-level "router" section.
type RouterConfig struct {
	// ReasoningEffortMap rewrites (or, mapped to "", removes) a top-level
	// string "reasoning_effort" field on every proxied request body. Absent
	// or empty disables rewriting entirely — the default, so behavior is
	// byte-identical to a settings.json with no "router" section at all.
	//
	// Keys and values are free-form strings on purpose, not a fixed
	// vocabulary: backends disagree about what they accept. Measured against
	// a real llama.cpp server (Qwen3.8-27B): "none" is accepted and turns
	// reasoning off; "low"/"medium"/"high"/"xhigh" are accepted and produce
	// reasoning; "minimal" 500s ("Unexpected reasoning effort minimal.
	// Supported types are xhigh (default), medium, and low."). Oh My Pi has
	// no wire value that means "off" — its `--thinking off` clamps to the
	// lowest entry in the model's configured `efforts` list and sends that
	// verbatim, so "minimal" is the lowest value such a client can be made
	// to send. Mapping {"minimal": "none"} turns that into the off signal
	// the backend actually understands. See CLAUDE.md's Relay-router section
	// for the full story.
	ReasoningEffortMap map[string]string `json:"reasoningEffortMap,omitempty"`

	// ReasoningEffortTemplateKwargs merges an object into a proxied body's
	// top-level "chat_template_kwargs" field when the request's ORIGINAL
	// "reasoning_effort" string value — matched the same way, and BEFORE,
	// ReasoningEffortMap rewrites it (see rewriteProxyBody) — matches a
	// configured key. Absent or empty disables this entirely, the default,
	// so behavior is unchanged from before this field existed.
	//
	// ReasoningEffortMap's value swap only fixes backends that interpret
	// reasoning_effort server-side (llama.cpp does). It does not fix oMLX:
	// measured at omlx/server.py:3594, oMLX merges request.reasoning_effort
	// verbatim into chat_template_kwargs and hands it to the model's Jinja
	// template — there is no server-side meaning to rewrite. The MLX build
	// of the measured model (CodeFast) uses the older Qwen convention
	// (enable_thinking), not reasoning_effort, so no VALUE swap of
	// reasoning_effort can reach it — the template never reads that field at
	// all. It needs a field-SHAPE rewrite: inject a different field.
	// Measured reasoning output length against oMLX CodeFast:
	//
	//	request                                            reasoning returned
	//	baseline                                            101 chars
	//	reasoning_effort: "none"                             94 chars — no effect
	//	chat_template_kwargs: {"enable_thinking": false}      0 chars — off
	//
	// The same chat_template_kwargs also turns reasoning off against
	// llama.cpp (0 chars, measured on "europa"), so each backend tolerates
	// the other's mechanism harmlessly — llama.cpp ignores an
	// unrecognized chat_template_kwargs key, oMLX ignores reasoning_effort
	// once nothing reads it. Configuring both knobs together (this field and
	// ReasoningEffortMap) is what makes "turn reasoning off" portable across
	// both backends from one client-side value.
	//
	// Matched against the value BEFORE ReasoningEffortMap's rewrite, not
	// after, because the two knobs describe ONE inbound client value
	// triggering TWO independent rewrites: configuring
	// {"minimal": "none"} (ReasoningEffortMap) alongside
	// {"minimal": {"enable_thinking": false}} (this field) must both fire
	// off the client's original "minimal". Matching post-rewrite would
	// require this field's keys to track whatever ReasoningEffortMap
	// happens to rewrite "minimal" INTO ("none") rather than what the client
	// actually sent — coupling the two maps together for no reason, and
	// breaking silently if either is reconfigured independently.
	//
	// Values are arbitrary JSON (bool, string, number, …), not just bools:
	// oMLX forwards chat_template_kwargs to the Jinja template verbatim, so
	// this passes values through untyped exactly like ReasoningEffortMap's
	// free-form string values do.
	//
	// The merge never overwrites a key the client's own body already sets
	// under chat_template_kwargs — mirroring oMLX's own
	// merged.setdefault(...) server-side, so the client's explicit choice
	// always wins over ours. See applyReasoningEffortTemplateKwargs.
	ReasoningEffortTemplateKwargs map[string]map[string]any `json:"reasoningEffortTemplateKwargs,omitempty"`

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
// own credential upstream (router.anthropic or router.passthrough). main
// uses it to refuse a plaintext non-loopback bind. StartRelayRouter uses it
// to start a router that has no local backends. Nil-safe.
func (c *RouterConfig) ForwardsClientCredentials() bool {
	return c != nil && (c.Anthropic != nil || len(c.Passthrough) > 0)
}

// AnthropicRouterConfig is settings.json's optional router.anthropic
// section. Absent entirely -> the /v1/messages routes 404 (RelayRouter.anthropic
// stays nil) — zero behavior change from before this feature existed, the
// same off-by-default shape as RouterConfig.ReasoningEffortMap.
type AnthropicRouterConfig struct {
	// Upstream is the real Anthropic API base URL passthrough forwards to.
	// Defaults to "https://api.anthropic.com" when empty.
	Upstream string `json:"upstream,omitempty"`

	// ModelMap redirects specific Anthropic model ids (exact, case-sensitive
	// match against the request body's top-level "model" field — same
	// free-form-string convention as RouterConfig.ReasoningEffortMap) to a
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
