package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// rewriteProxyBody applies the router's field-level body rewrites in one
// decode/encode pass, so an endpoint-routed body is never unmarshalled and
// remarshalled twice for independent rewrites. model, when non-empty,
// replaces the top-level "model" field (endpoint routes rewrite
// "endpoint.Name/id" down to the bare id the endpoint itself expects).
// effortMap, when non-empty,
// rewrites or removes a top-level string "reasoning_effort" field; see
// applyReasoningEffortMap. templateKwargsMap, when non-empty, merges an
// object into a top-level "chat_template_kwargs" field; see
// applyReasoningEffortTemplateKwargs.
//
// When all three are no-ops (no model swap, no configured maps) the body is
// returned completely untouched rather than round-tripped through
// encoding/json — that's load-bearing for the managed-alias route, which has
// no model to swap: with neither map configured (the default), it must stay
// byte-identical to before these features existed, not just semantically
// unchanged with reordered keys.
//
// RawMessage avoids re-marshalling nested payloads verbatim (large
// image_url parts, ordered tool definitions, etc.) when a rewrite does
// happen. Top-level key order is not preserved in that case: json.Marshal of
// a map sorts keys.
func rewriteProxyBody(body []byte, model string, effortMap map[string]string, templateKwargsMap map[string]map[string]any) ([]byte, error) {
	if model == "" && len(effortMap) == 0 && len(templateKwargsMap) == 0 {
		return body, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	if model != "" {
		encoded, err := json.Marshal(model)
		if err != nil {
			return nil, fmt.Errorf("encode upstream id: %w", err)
		}
		raw["model"] = encoded
	}

	// Capture the client's ORIGINAL reasoning_effort value before
	// applyReasoningEffortMap gets a chance to rewrite or remove it. Both
	// reasoning_effort rewrites key off this same original value — see
	// RouterConfig.ReasoningEffortTemplateKwargs for why matching after the
	// value-map swap would be wrong: {"minimal":"none"} (effortMap) and
	// {"minimal":{"enable_thinking":false}} (templateKwargsMap) describe two
	// independent reactions to ONE client value, "minimal". Reading the
	// field again after applyReasoningEffortMap ran would see "none" instead
	// (or nothing, if minimal maps to removal), silently breaking that
	// combination.
	effort, hasEffort := reasoningEffortValue(raw)

	applyReasoningEffortMap(raw, effortMap)
	if hasEffort {
		applyReasoningEffortTemplateKwargs(raw, templateKwargsMap, effort)
	}
	return json.Marshal(raw)
}

// reasoningEffortValue reads raw's top-level "reasoning_effort" field as a
// string, reporting ok=false when the field is absent or not a JSON string.
// Factored out of applyReasoningEffortMap so rewriteProxyBody can capture
// the field's value BEFORE that function has a chance to mutate or remove
// it — see rewriteProxyBody's comment on why the original value is what
// applyReasoningEffortTemplateKwargs must match against.
func reasoningEffortValue(raw map[string]json.RawMessage) (string, bool) {
	rawEffort, ok := raw["reasoning_effort"]
	if !ok {
		return "", false
	}
	var effort string
	if err := json.Unmarshal(rawEffort, &effort); err != nil {
		return "", false // not a JSON string — leave whatever it is alone.
	}
	return effort, true
}

// applyReasoningEffortMap rewrites, or removes, a top-level string
// "reasoning_effort" field in raw per effortMap — see RouterConfig for why
// this exists. Mutates raw in place; a no-op effortMap or a body with
// nothing to rewrite leaves raw untouched.
//
// Only an exact, case-sensitive match on the field's current string value is
// rewritten. Everything else passes through deliberately: a missing field, a
// non-string value (some other client sending a number isn't ours to
// interpret), and a string that isn't a configured key (it may already be
// exactly what the backend wants).
func applyReasoningEffortMap(raw map[string]json.RawMessage, effortMap map[string]string) {
	if len(effortMap) == 0 {
		return
	}
	effort, ok := reasoningEffortValue(raw)
	if !ok {
		return
	}
	mapped, ok := effortMap[effort]
	if !ok {
		return
	}
	if mapped == "" {
		// Some backends reject an empty string outright; "omit the field" is
		// a distinct, useful outcome from "set it to none" — see RouterConfig.
		delete(raw, "reasoning_effort")
		slog.Debug("relay router: removed reasoning_effort", "from", effort)
		return
	}
	encoded, err := json.Marshal(mapped)
	if err != nil {
		return // unreachable in practice (marshalling a string cannot fail)
	}
	raw["reasoning_effort"] = encoded
	slog.Debug("relay router: rewrote reasoning_effort", "from", effort, "to", mapped)
}

// applyReasoningEffortTemplateKwargs merges kwargsMap[effort] into raw's
// top-level "chat_template_kwargs" object, creating it if absent. effort is
// the client's ORIGINAL "reasoning_effort" value — the caller (
// rewriteProxyBody) captures it before applyReasoningEffortMap runs, per
// RouterConfig.ReasoningEffortTemplateKwargs's doc comment. Mutates raw in
// place; a no-op kwargsMap, a non-matching effort, or an empty configured
// object leave raw untouched.
//
// A key the merge would set is left alone if raw's existing
// chat_template_kwargs already defines it — mirroring oMLX's own
// merged.setdefault(...) server-side, so a client's explicit choice always
// wins over ours (see RouterConfig).
func applyReasoningEffortTemplateKwargs(raw map[string]json.RawMessage, kwargsMap map[string]map[string]any, effort string) {
	if len(kwargsMap) == 0 {
		return
	}
	kwargs, ok := kwargsMap[effort]
	if !ok || len(kwargs) == 0 {
		return
	}

	existing := map[string]json.RawMessage{}
	if rawExisting, present := raw["chat_template_kwargs"]; present {
		if err := json.Unmarshal(rawExisting, &existing); err != nil {
			// Not a JSON object — a malformed client body isn't ours to fix;
			// leave it untouched rather than clobbering it with our own.
			return
		}
	}

	changed := false
	for k, v := range kwargs {
		if _, present := existing[k]; present {
			continue // client-supplied value wins — setdefault semantics.
		}
		encoded, err := json.Marshal(v)
		if err != nil {
			continue // unreachable: config values decode from JSON already
		}
		existing[k] = encoded
		changed = true
	}
	if !changed {
		return
	}
	encoded, err := json.Marshal(existing)
	if err != nil {
		return // unreachable: existing is built entirely from valid RawMessages
	}
	raw["chat_template_kwargs"] = encoded
	slog.Debug("relay router: merged reasoning_effort template kwargs", "reasoning_effort", effort, "keys", kwargs)
}
