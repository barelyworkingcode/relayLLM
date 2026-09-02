package main

// OpenAIEndpoint describes a single OpenAI-compatible chat completions server.
// Multiple endpoints can be configured side-by-side (Ollama's /v1, LM Studio,
// OMLX, OpenAI proper, etc.). The Name field is used as a routing prefix on
// model identifiers (e.g. "lmstudio/qwen2.5-7b").
//
// Set Strict=true for endpoints that 400 on unknown body fields (OpenAI proper
// in some API versions, Azure OpenAI, stricter gateways). When strict, the
// transport omits stream_options and the non-standard sampling fields (top_k,
// min_p, repetition_penalty). Defaults to false to preserve compatibility
// with the wider compat-server ecosystem (LM Studio, Ollama /v1, oMLX), which
// accepts those fields silently.
type OpenAIEndpoint struct {
	Name    string `json:"name"`             // routing prefix, e.g. "lmstudio"
	BaseURL string `json:"baseURL"`          // e.g. "http://localhost:1234/v1"
	APIKey  string `json:"apiKey"`           // optional; sent as "Authorization: Bearer ..."
	Group   string `json:"group"`            // display group in model picker; defaults to Name
	Strict  bool   `json:"strict,omitempty"` // gate non-standard request fields
}

// OpenAIConfig is the top-level config file structure.
type OpenAIConfig struct {
	Endpoints []OpenAIEndpoint `json:"endpoints"`
}

// VirtualLLMConfig defines user-facing model names that attempt an ordered
// list of fallback targets. Targets can refer to configured OpenAI endpoints
// or managed-server aliases on this relayLLM host. Declared order is a
// preference, not a hard gate: RelayRouter.candidatesForVirtual prefers
// targets it currently believes are reachable, but still attempts the rest
// as a last resort, and RelayRouter.routeVirtual retries the next candidate
// on any pre-response failure — see relay_router.go for why (the reachability
// cache is 15s stale by design).
type VirtualLLMConfig struct {
	Models []VirtualLLM `json:"models"`
}

// VirtualLLM is one stable model name (for example, "vCode") with ordered
// fallback targets. A target either names an OpenAI endpoint plus its bare
// upstream model id, or names a managed-server alias on this relayLLM host.
// The name always appears in /v1/models — even when every target is
// currently unreachable — because the point of a virtual name is that it's
// stable config a client can poll and dispatch against.
type VirtualLLM struct {
	Name    string             `json:"name"`
	Targets []VirtualLLMTarget `json:"targets"`
}

type VirtualLLMTarget struct {
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
	Alias    string `json:"alias"`
}

func (c *VirtualLLMConfig) Find(name string) *VirtualLLM {
	if c == nil {
		return nil
	}
	for i := range c.Models {
		if c.Models[i].Name == name {
			return &c.Models[i]
		}
	}
	return nil
}

// Find returns a pointer to the endpoint with the given Name, or nil if none
// matches. Comparison is case-sensitive to match how model prefixes are
// parsed elsewhere.
func (c *OpenAIConfig) Find(name string) *OpenAIEndpoint {
	if c == nil {
		return nil
	}
	for i := range c.Endpoints {
		if c.Endpoints[i].Name == name {
			return &c.Endpoints[i]
		}
	}
	return nil
}

// Names returns the list of endpoint names, in declaration order. Used by
// session routing to decide whether a "prefix/model" string refers to a
// configured OpenAI endpoint or falls through to the Ollama provider.
func (c *OpenAIConfig) Names() []string {
	if c == nil {
		return nil
	}
	names := make([]string, len(c.Endpoints))
	for i, e := range c.Endpoints {
		names[i] = e.Name
	}
	return names
}
