package main

// SettingField describes a single configurable parameter for a provider.
type SettingField struct {
	Key         string      `json:"key"`
	Label       string      `json:"label"`
	Type        string      `json:"type"`          // "number", "boolean", "string", "string[]"
	Default     interface{} `json:"default"`
	Min         *float64    `json:"min,omitempty"`
	Max         *float64    `json:"max,omitempty"`
	Step        *float64    `json:"step,omitempty"`
	Placeholder string      `json:"placeholder,omitempty"`
	Hint        string      `json:"hint,omitempty"`
}

func ptr(f float64) *float64 { return &f }

// ProviderSettings returns the settings schema for each provider.
func ProviderSettings() map[string][]SettingField {
	return map[string][]SettingField{
		"claude": {},
		"lmstudio": {
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
				Key:     "reasoning",
				Label:   "Reasoning",
				Type:    "boolean",
				Default: false,
				Hint:    "Enable extended thinking / chain-of-thought.",
			},
			{
				Key:   "contextLength",
				Label: "Context Length",
				Type:  "number",
				Hint:  "Max context window size. Leave empty for model default.",
			},
			{
				Key:         "integrations",
				Label:       "MCP Integrations",
				Type:        "string[]",
				Placeholder: "e.g. mcp/relay mcp/filesystem",
				Hint:        "Space-separated MCP server references from mcp.json.",
			},
		},
	}
}
