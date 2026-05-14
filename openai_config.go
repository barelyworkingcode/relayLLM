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
