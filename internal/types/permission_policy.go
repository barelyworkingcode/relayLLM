package types

// PermissionPolicy is the per-session Claude permission policy. Sourced from
// session.Settings.permissionPolicy at session creation. relayLLM uses it for
// (a) Claude CLI flags at spawn time and (b) short-circuit evaluation in
// RegisterPermissionRoutes (matched rules skip the WS roundtrip to Eve).
type PermissionPolicy struct {
	DefaultMode  string   `json:"defaultMode,omitempty"`
	AllowedTools []string `json:"allowedTools,omitempty"`
	DeniedTools  []string `json:"deniedTools,omitempty"`
}
