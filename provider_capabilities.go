package main

// ProviderCapabilities reports static feature support per provider type.
// Surfaced in the model catalog so clients can gate UI features without
// guessing from the provider string.
type ProviderCapabilities struct {
	SupportsPermissions bool `json:"supportsPermissions"`
	SupportsAttachments bool `json:"supportsAttachments"`
}

// CapabilitiesForProvider returns capabilities for a provider type string
// as used by Session.ProviderType ("claude", "openai", "ollama", "llama").
// Permission mode is Claude-only because it's wired through the Claude CLI's
// --permission-mode flag and has no equivalent in the OpenAI/Ollama paths.
func CapabilitiesForProvider(providerType string) ProviderCapabilities {
	switch providerType {
	case "claude":
		return ProviderCapabilities{SupportsPermissions: true, SupportsAttachments: true}
	case "openai", "ollama", "pi":
		return ProviderCapabilities{SupportsAttachments: true}
	default:
		return ProviderCapabilities{}
	}
}
