package types

// ModelInfo describes one selectable model row returned by GET /api/models.
type ModelInfo struct {
	Label               string `json:"label"`
	Value               string `json:"value"`
	Group               string `json:"group"`
	Provider            string `json:"provider"`
	SupportsPermissions bool   `json:"supportsPermissions"`
	SupportsAttachments bool   `json:"supportsAttachments"`
}
