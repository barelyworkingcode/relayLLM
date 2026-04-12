package main

// SettingField describes a single configurable parameter for a provider.
type SettingField struct {
	Key         string      `json:"key"`
	Label       string      `json:"label"`
	Type        string      `json:"type"` // "number", "boolean", "string", "string[]", "select"
	Default     interface{} `json:"default"`
	Min         *float64    `json:"min,omitempty"`
	Max         *float64    `json:"max,omitempty"`
	Step        *float64    `json:"step,omitempty"`
	Options     []string    `json:"options,omitempty"`
	Placeholder string      `json:"placeholder,omitempty"`
	Hint        string      `json:"hint,omitempty"`
}

func ptr(f float64) *float64 { return &f }

// ProviderSettings returns the settings schema for each provider.
func ProviderSettings() map[string][]SettingField {
	ollamaFields := []SettingField{
		{
			Key:     "temperature",
			Label:   "Temperature",
			Type:    "number",
			Default: 0.7,
			Min:     ptr(0),
			Max:     ptr(2),
			Step:    ptr(0.1),
			Hint:    "Controls randomness. Lower = more focused, higher = more creative.",
		},
		{
			Key:   "top_p",
			Label: "Top P",
			Type:  "number",
			Min:   ptr(0),
			Max:   ptr(1),
			Step:  ptr(0.05),
			Hint:  "Nucleus sampling. Only consider tokens with cumulative probability <= top_p.",
		},
		{
			Key:   "top_k",
			Label: "Top K",
			Type:  "number",
			Min:   ptr(0),
			Step:  ptr(1),
			Hint:  "Limits token selection to top K candidates.",
		},
		{
			Key:   "min_p",
			Label: "Min P",
			Type:  "number",
			Min:   ptr(0),
			Max:   ptr(1),
			Step:  ptr(0.01),
			Hint:  "Minimum probability threshold relative to the top token.",
		},
		{
			Key:     "think",
			Label:   "Thinking",
			Type:    "boolean",
			Default: false,
			Hint:    "Enable extended thinking / chain-of-thought.",
		},
		{
			Key:   "num_ctx",
			Label: "Context Length",
			Type:  "number",
			Hint:  "Max context window size. Leave empty for model default.",
		},
		{
			Key:     "useRelayTools",
			Label:   "Use Relay Tools",
			Type:    "boolean",
			Default: false,
			Hint:    "Enable Relay MCP tools (email, calendar, contacts, web search, etc.)",
		},
		{
			Key:   "mcpServers",
			Label: "MCP Servers",
			Type:  "json",
			Hint:  "MCP server configurations for tool calling. Same format as Claude Desktop.",
		},
	}

	openaiFields := []SettingField{
		{
			Key:     "temperature",
			Label:   "Temperature",
			Type:    "number",
			Default: 0.7,
			Min:     ptr(0),
			Max:     ptr(2),
			Step:    ptr(0.1),
			Hint:    "Controls randomness. Lower = more focused, higher = more creative.",
		},
		{
			Key:   "top_p",
			Label: "Top P",
			Type:  "number",
			Min:   ptr(0),
			Max:   ptr(1),
			Step:  ptr(0.05),
			Hint:  "Nucleus sampling. Only consider tokens with cumulative probability <= top_p.",
		},
		{
			Key:   "top_k",
			Label: "Top K",
			Type:  "number",
			Min:   ptr(0),
			Step:  ptr(1),
			Hint:  "Limits token selection to top K candidates. Non-standard OpenAI extension; ignored by servers that don't support it.",
		},
		{
			Key:     "useRelayTools",
			Label:   "Use Relay Tools",
			Type:    "boolean",
			Default: false,
			Hint:    "Enable Relay MCP tools (email, calendar, contacts, web search, etc.)",
		},
		{
			Key:   "mcpServers",
			Label: "MCP Servers",
			Type:  "json",
			Hint:  "MCP server configurations for tool calling. Same format as Claude Desktop.",
		},
	}

	return map[string][]SettingField{
		"claude": {},
		"ollama": ollamaFields,
		"openai": openaiFields,
	}
}
