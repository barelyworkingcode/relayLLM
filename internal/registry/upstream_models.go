package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"relayllm/internal/config"
)

// UpstreamModel is one model offered by a configured OpenAI-compatible
// endpoint. ContextLength is 0 when the upstream does not advertise one.
type UpstreamModel struct {
	ID            string
	ContextLength int64
	// SupportsImages is true only when the upstream said so. Plain OpenAI
	// /v1/models carries no modality field, so false means "not advertised",
	// not "proven text-only".
	SupportsImages bool
}

// modelsFetchTimeout must stay comfortably above the worst-case /models
// latency of an upstream that is itself a relayLLM router. Such an upstream
// answers /models only after probing its own endpoints, so a cold cache costs
// it a full probe timeout before it replies. Setting this to that same probe
// timeout makes a router-in-front-of-a-router flap permanently: every probe
// loses the race by milliseconds, the endpoint is recorded offline, and its
// models never appear. Shrink this and chained routers stop seeing each other.
// Connect itself is bounded separately, at 1s, by endpoint.ProbeTransport().
const modelsFetchTimeout = 10 * time.Second

// FetchOpenAIModels queries /v1/models on the endpoint and returns the raw
// upstream models (IDs carry no endpoint prefix). The error return distinguishes
// "endpoint unreachable / unhealthy" from "endpoint healthy but empty" so the
// ProxyRegistry can record online/offline state accurately.
func FetchOpenAIModels(ctx context.Context, endpoint config.OpenAIEndpoint) ([]UpstreamModel, error) {
	client := &http.Client{Timeout: modelsFetchTimeout, Transport: endpoint.ProbeTransport()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.BaseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if endpoint.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+endpoint.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("non-OK status %d", resp.StatusCode)
	}
	var result struct {
		Data []upstreamModelRow `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	models := make([]UpstreamModel, 0, len(result.Data))
	for _, m := range result.Data {
		models = append(models, UpstreamModel{
			ID:             m.ID,
			ContextLength:  m.contextLength(),
			SupportsImages: m.supportsImages(),
		})
	}
	return models, nil
}

// upstreamModelRow is one entry of an upstream /v1/models response. Only `id`
// is standard OpenAI; the context-length fields are server-specific extensions
// that we read opportunistically so clients get a real context window instead
// of a default.
type upstreamModelRow struct {
	ID string `json:"id"`

	// Context length under the name each server family happens to use.
	MaxModelLen      int64 `json:"max_model_len"`      // vLLM, OMLX
	MaxContextLength int64 `json:"max_context_length"` // LM Studio
	ContextLength    int64 `json:"context_length"`     // TGI, some gateways
	ContextWindow    int64 `json:"context_window"`     // misc
	Meta             *struct {
		NCtx      int64 `json:"n_ctx"`
		NCtxTrain int64 `json:"n_ctx_train"`
	} `json:"meta"` // llama.cpp

	// llama.cpp router mode. Read opportunistically for the same reason as the
	// context fields: an upstream that declares its modalities lets us tell a
	// VLM from a text model, which plain OpenAI /v1/models cannot express.
	Architecture *struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
}

// supportsImages reports whether the upstream explicitly advertised image
// input. Absent the field we say no — claiming vision a server does not have
// makes clients send images that come back as errors mid-turn.
func (m upstreamModelRow) supportsImages() bool {
	if m.Architecture == nil {
		return false
	}
	for _, mod := range m.Architecture.InputModalities {
		if mod == "image" {
			return true
		}
	}
	return false
}

// contextLength returns the first context figure the row actually carries,
// or 0 when the server advertises none.
func (m upstreamModelRow) contextLength() int64 {
	candidates := []int64{m.MaxModelLen, m.MaxContextLength, m.ContextLength, m.ContextWindow}
	if m.Meta != nil {
		candidates = append(candidates, m.Meta.NCtx, m.Meta.NCtxTrain)
	}
	for _, c := range candidates {
		if c > 0 {
			return c
		}
	}
	return 0
}
