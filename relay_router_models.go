package main

import (
	"fmt"
	"net/http"
	"relayllm/internal/config"
	"sort"
	"strings"
)

// handleModels serves the catalog for both /v1/models and /models.
//
// Rows carry llama.cpp router mode's extra fields (status, meta, architecture)
// alongside the OpenAI ones. That is deliberate: clients written against
// llama.cpp's router — pi ships a built-in extension that does exactly this —
// validate every row has a string `status.value` and reject the whole catalog
// without it. The fields are additive, so plain OpenAI clients ignore them.
func (p *RelayRouter) handleModels(w http.ResponseWriter, r *http.Request) {
	var data []map[string]any
	// Dispatch resolves a model id to exactly one behavior — managed alias,
	// then virtual name, then endpoint model, in that priority order (see
	// handleProxy) — so every row type shares this dedup set, built in that
	// same order, and the row that survives here is the one that will
	// actually serve a request for that id. A collision with a managed alias
	// is dead config no matter which section declares it: managers are
	// checked first in handleProxy regardless of catalog order. A collision
	// between a virtual name and an endpoint's prefixed id is NOT symmetric
	// the same way — handleProxy checks p.virtual.Find before
	// p.registry.LookupModel, so it's always the endpoint side that's
	// unreachable, never the virtual side. Building rows in dispatch order
	// (managed, virtual, endpoint — it used to be managed, endpoint, virtual)
	// is what keeps this listing honest about which one that is.
	seen := make(map[string]bool)
	for _, m := range p.managers {
		for _, entry := range m.ModelCatalog() {
			if seen[entry.Alias] {
				continue
			}
			seen[entry.Alias] = true

			status := map[string]any{"value": entry.Status}
			if entry.Failed {
				// Clients poll until "loaded"; without a failure flag a model
				// that can never start would be polled forever.
				status["failed"] = true
				if entry.Error != "" {
					status["error"] = entry.Error
				}
			}

			modalities := []string{"text"}
			if entry.SupportsImages {
				modalities = append(modalities, "image")
			}

			row := map[string]any{
				"id":           entry.Alias,
				"object":       "model",
				"created":      0,
				"owned_by":     m.profile.Group,
				"status":       status,
				"architecture": map[string]any{"input_modalities": modalities},
			}
			// n_ctx is what the server will actually run with; n_ctx_train is
			// the model's native limit. Clients read n_ctx first and fall back
			// to n_ctx_train, so a model with no pinned ctx-size still reports
			// a real number instead of the client's generic default.
			meta := map[string]any{}
			if entry.ContextSize > 0 {
				meta["n_ctx"] = entry.ContextSize
			}
			if entry.TrainedContext > 0 {
				meta["n_ctx_train"] = entry.TrainedContext
			}
			if len(meta) > 0 {
				row["meta"] = meta
			}
			// context_length mirrors the same number at the top level, for
			// clients that discover models the plain-OpenAI way (LM Studio-,
			// vLLM-, OpenRouter-style) and never look inside meta at all —
			// Oh My Pi's openai-models-list branch is one, and its fallback
			// on a missing field is a hardcoded 128K, not an error. That's
			// silently wrong for a model actually pinned smaller: a client
			// that trusts it builds an oversized request and fails mid-turn.
			// Omitted, never zero, when neither number is known, so `??
			// default` in the client falls through instead of landing on an
			// invented context window.
			if v, ok := resolveContextLength(entry.ContextSize, entry.TrainedContext); ok {
				row["context_length"] = v
			}
			data = append(data, row)
		}
	}
	// Snapshotted once and reused for every virtual row below AND every
	// endpoint row further down — Snapshot is O(endpoints); probing it again
	// per virtual model would make this handler O(virtuals × endpoints) for
	// no benefit, since every virtual model shares the same registry state.
	var epStatuses []EndpointStatus
	if p.registry != nil {
		epStatuses = p.registry.Snapshot(r.Context())
	}

	// Virtual rows come before endpoint rows: dispatch (handleProxy) checks
	// p.virtual.Find before p.registry.LookupModel, so a virtual name that
	// happens to collide with an endpoint's prefixed id (e.g. a virtual
	// literally named "ep/model") must win the dedup here too, or the
	// catalog would list a row a request for that id would never actually
	// reach.
	if p.virtual != nil {
		for i := range p.virtual.Models {
			virtual := &p.virtual.Models[i]
			if seen[virtual.Name] {
				continue
			}
			seen[virtual.Name] = true
			// A virtual name is stable config — unlike an endpoint model, it
			// doesn't disappear from the catalog just because its targets are
			// all offline right now. It reports unloaded+failed instead, so a
			// client polling for readiness stops rather than spinning forever
			// on a name that will never resolve.
			data = append(data, p.virtualCatalogRow(virtual, epStatuses))
		}
	}

	for _, status := range epStatuses {
		if !status.Online {
			continue
		}
		for _, m := range status.Models {
			id := status.Endpoint.Name + "/" + m.ID
			if seen[id] {
				continue
			}
			seen[id] = true
			row := map[string]any{
				"id":       id,
				"object":   "model",
				"created":  0,
				"owned_by": status.Endpoint.Name,
				// Remote endpoints have no load step and the registry has
				// already dropped the unreachable ones, so anything listed
				// here is usable right now.
				"status": map[string]any{"value": ModelStatusLoaded},
				// Text unless the upstream advertised otherwise: plain
				// OpenAI /v1/models has no modality field, so a VLM behind
				// an endpoint that stays quiet is indistinguishable from a
				// text model. Emitting the key either way keeps every row
				// the same shape for clients that read
				// architecture.input_modalities unconditionally.
				"architecture": map[string]any{"input_modalities": endpointModalities(m)},
			}
			// Only when the upstream actually advertised one — omitting the
			// field lets the client apply its own default rather than
			// trusting a number we invented. context_length is the flat
			// counterpart to meta.n_ctx above, for the same reason it
			// exists on managed rows: see the comment there.
			if v, ok := resolveContextLength(m.ContextLength); ok {
				row["meta"] = map[string]any{"n_ctx": v}
				row["context_length"] = v
			}
			data = append(data, row)
		}
	}

	// Anthropic-compat modelMap keys (see relay_router_anthropic.go) are
	// also dispatchable via the plain OpenAI path (handleProxy resolves the
	// map before any other check), so they belong in the catalog too — a
	// client would otherwise see a 400 "unknown model" for an id the router
	// actually serves. Sorted for deterministic output; iterating a map
	// directly here would make this handler's response order flap.
	if p.anthropic != nil && len(p.anthropic.modelMap) > 0 {
		keys := make([]string, 0, len(p.anthropic.modelMap))
		for key := range p.anthropic.modelMap {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if seen[key] {
				continue
			}
			seen[key] = true
			data = append(data, map[string]any{
				"id":           key,
				"object":       "model",
				"created":      0,
				"owned_by":     "anthropic-map",
				"status":       map[string]any{"value": ModelStatusLoaded},
				"architecture": map[string]any{"input_modalities": []string{"text"}},
			})
		}
	}

	if data == nil {
		data = []map[string]any{}
	}

	writeRouterJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

// handleModelLoad starts loading a managed model and returns immediately —
// see ServerManager.StartLoad for why this must not block.

func (p *RelayRouter) virtualCatalogRow(virtual *config.VirtualLLM, statuses []EndpointStatus) map[string]any {
	candidates, freshCount := candidatesForVirtual(virtual, statuses, p.managers)

	modalities := []string{"text"}
	var meta map[string]any
	var status map[string]any
	var contextLength int64
	var hasContextLength bool
	if freshCount > 0 {
		status = map[string]any{"value": ModelStatusLoaded}
		modalities, meta, contextLength, hasContextLength = virtualRowMetadata(candidates[0], statuses)
	} else {
		status = map[string]any{
			"value":  ModelStatusUnloaded,
			"failed": true,
			// Same convention as a managed alias's load failure: without this
			// a client polling for "loaded" spins forever instead of stopping.
			"error": virtualUnavailableReason(virtual, statuses),
		}
		if len(candidates) > 0 {
			// Still inherit metadata from the best last-resort candidate —
			// it's what a request would actually hit if it succeeded.
			modalities, meta, contextLength, hasContextLength = virtualRowMetadata(candidates[0], statuses)
		}
	}

	row := map[string]any{
		"id":           virtual.Name,
		"object":       "model",
		"created":      0,
		"owned_by":     "virtual",
		"status":       status,
		"architecture": map[string]any{"input_modalities": modalities},
	}
	if len(meta) > 0 {
		row["meta"] = meta
	}
	// context_length inherits the same precedence as a managed row's — see
	// the comment in handleModels — because a virtual name resolves to
	// whatever candidates[0] is, managed alias or endpoint target alike.
	if hasContextLength {
		row["context_length"] = contextLength
	}
	return row
}

// virtualRowMetadata inherits architecture/meta/context_length from a virtual
// model's first attempt-order candidate — the target dispatch will actually
// try first. A managed alias's metadata is a config fact, available whether
// or not the server is currently running; an endpoint target's metadata only
// exists when its last probe succeeded, so an offline candidate falls back to
// the same text-only/no-meta defaults the rest of /v1/models uses for
// anything unadvertised. Never claim "image" support that can't be backed —
// offering images to a server that can't take them fails mid-turn (see
// CLAUDE.md).
func virtualRowMetadata(first resolvedVirtualTarget, statuses []EndpointStatus) (modalities []string, meta map[string]any, contextLength int64, hasContextLength bool) {
	if first.manager != nil {
		for _, entry := range first.manager.ModelCatalog() {
			if entry.Alias != first.alias {
				continue
			}
			modalities = []string{"text"}
			if entry.SupportsImages {
				modalities = append(modalities, "image")
			}
			metaOut := map[string]any{}
			if entry.ContextSize > 0 {
				metaOut["n_ctx"] = entry.ContextSize
			}
			if entry.TrainedContext > 0 {
				metaOut["n_ctx_train"] = entry.TrainedContext
			}
			contextLength, hasContextLength = resolveContextLength(entry.ContextSize, entry.TrainedContext)
			if len(metaOut) == 0 {
				return modalities, nil, contextLength, hasContextLength
			}
			return modalities, metaOut, contextLength, hasContextLength
		}
		return []string{"text"}, nil, 0, false
	}

	for _, status := range statuses {
		if status.Endpoint.Name != first.endpoint.Name {
			continue
		}
		for _, m := range status.Models {
			if m.ID != first.upstreamID {
				continue
			}
			var metaOut map[string]any
			contextLength, hasContextLength = resolveContextLength(m.ContextLength)
			if hasContextLength {
				metaOut = map[string]any{"n_ctx": contextLength}
			}
			return endpointModalities(m), metaOut, contextLength, hasContextLength
		}
	}
	return []string{"text"}, nil, 0, false
}

// resolveContextLength picks the single number a catalog row's flat
// context_length field reports, in priority order: the first positive value
// wins. For a managed model that's (ContextSize, TrainedContext) — the
// pinned ctx-size is what the server will actually run with, the trained
// context is the honest fallback for a model with no pin — the same
// precedence as the meta block's n_ctx/n_ctx_train pair. For an endpoint
// model there's only ever one number (ContextLength). One function serves
// both because context_length is flat: unlike meta, it has no room to carry
// two numbers, so every row type must collapse to this single call.
func resolveContextLength(candidates ...int64) (int64, bool) {
	for _, v := range candidates {
		if v > 0 {
			return v, true
		}
	}
	return 0, false
}

// virtualUnavailableReason explains, for the catalog's status.error field,
// why a virtual model currently has no target believed usable. It only
// describes reachability (offline endpoints, missing aliases) — config
// mistakes (bad target shape, unknown endpoint/alias names) are
// warnVirtualModelConfig's job at startup, not a per-request runtime message.
func virtualUnavailableReason(virtual *config.VirtualLLM, statuses []EndpointStatus) string {
	configured := make(map[string]bool, len(statuses))
	for _, status := range statuses {
		configured[status.Endpoint.Name] = true
	}
	var reasons []string
	for _, target := range virtual.Targets {
		switch {
		case target.Alias != "":
			reasons = append(reasons, fmt.Sprintf("alias %q not available", target.Alias))
		case target.Endpoint != "" && target.Model != "" && configured[target.Endpoint]:
			reasons = append(reasons, fmt.Sprintf("endpoint %q offline", target.Endpoint))
		}
	}
	if len(reasons) == 0 {
		return "no usable target configured"
	}
	return "no target currently reachable: " + strings.Join(reasons, "; ")
}

// endpointModalities renders an upstream model's advertised input modalities.
// Text is always present: every chat model takes text, and a client that finds
// an empty list has nothing to fall back on.
func endpointModalities(m UpstreamModel) []string {
	if m.SupportsImages {
		return []string{"text", "image"}
	}
	return []string{"text"}
}
