package main

import (
	"encoding/json"
	"log/slog"
	"os"
)

// Manifest is the wire shape relayLLM declares to relay. Must stay
// field-compatible with relay/bridge/manifest.go (which is in a separate
// Go module, so the types are intentionally mirrored here).
type Manifest struct {
	Routes    []string       `json:"routes"`
	Status    *StatusDecl    `json:"status,omitempty"`
	Actions   []ActionDecl   `json:"actions,omitempty"`
	Resources []ResourceDecl `json:"resources,omitempty"`
}

type StatusDecl struct {
	Path string `json:"path"`
}

type ActionDecl struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Method       string `json:"method"`
	PathTemplate string `json:"pathTemplate"`
	// ForEach names a top-level array key in the status payload. When set,
	// the UI renders one button per row in that array and substitutes the
	// row's keys into PathTemplate's {placeholders}. Empty = global action.
	ForEach string `json:"forEach,omitempty"`
}

// ResourceDecl declares a typed collection that relay's Service Inspector can
// CRUD. Mirrors relay/bridge/manifest.go's ResourceDecl wire shape.
type ResourceDecl struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Help  string `json:"help,omitempty"`

	List   EndpointDecl  `json:"list"`
	Create *EndpointDecl `json:"create,omitempty"`
	Update *EndpointDecl `json:"update,omitempty"`
	Delete *EndpointDecl `json:"delete,omitempty"`

	Fields         []FieldDecl `json:"fields"`
	ProtectedField string      `json:"protectedField,omitempty"`
}

type EndpointDecl struct {
	Method       string `json:"method"`
	PathTemplate string `json:"pathTemplate"`
}

type FieldDecl struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
	Required    bool   `json:"required,omitempty"`
	ReadOnly    bool   `json:"readOnly,omitempty"`
	HideInTable bool   `json:"hideInTable,omitempty"`
}

// registerManifestRequest is the Arguments payload for ReqRegisterManifest.
type registerManifestRequest struct {
	ServiceID      string   `json:"serviceId"`
	Manifest       Manifest `json:"manifest"`
	InternalSocket string   `json:"internalSocket"`
	InternalToken  string   `json:"internalToken"`
}

// buildManifest declares the routes, status endpoint, and user actions
// relayLLM wants relay to dispatch / surface. Kept narrow on purpose —
// see ../relay/plans/service-manifest-spec.md.
func buildManifest() Manifest {
	return Manifest{
		Routes: []string{
			"/api/sessions",
			"/api/sessions/",
			"/api/models",
			"/api/terminals",
			"/api/terminals/",
			"/api/terminal/",
			"/api/permission",
			"/api/generated/",
			"/api/status",
			"/api/llama/",
			"/ws",
		},
		Status: &StatusDecl{
			Path: "/api/status",
		},
		Actions: []ActionDecl{
			{
				ID:           "stop-llama",
				Label:        "Stop",
				Method:       "DELETE",
				PathTemplate: "/api/llama/instances/{alias}",
				ForEach:      "instances",
			},
			{
				ID:           "stop-terminal",
				Label:        "Kill",
				Method:       "DELETE",
				PathTemplate: "/api/terminals/{id}",
				ForEach:      "terminals",
			},
		},
		Resources: []ResourceDecl{
			{
				ID:    "pty_templates",
				Label: "Terminal Templates",
				Help:  "Launchable terminal types. Built-ins (claude-code, opencode, shell) cannot be edited or removed.",
				List:  EndpointDecl{Method: "GET", PathTemplate: "/api/terminal/templates"},
				Create: &EndpointDecl{Method: "POST", PathTemplate: "/api/terminal/templates"},
				Update: &EndpointDecl{Method: "PUT", PathTemplate: "/api/terminal/templates/{id}"},
				Delete: &EndpointDecl{Method: "DELETE", PathTemplate: "/api/terminal/templates/{id}"},
				Fields: []FieldDecl{
					{ID: "name", Label: "Name", Type: "text", Required: true, Placeholder: "e.g. My REPL"},
					{ID: "command", Label: "Command", Type: "text", Required: true, Placeholder: "e.g. /usr/local/bin/myrepl"},
					{ID: "args", Label: "Args", Type: "string[]", Help: "One per line."},
					{ID: "env", Label: "Environment", Type: "stringMap", Help: "KEY=VALUE per line."},
					{ID: "icon", Label: "Icon", Type: "text", Placeholder: "terminal", HideInTable: true},
					{ID: "description", Label: "Description", Type: "textarea", HideInTable: true},
					{ID: "builtIn", Label: "Built-in", Type: "bool", ReadOnly: true},
				},
				ProtectedField: "builtIn",
			},
		},
	}
}

// maybeRegisterManifest tells relay where to dispatch front-door traffic
// for this service. Standalone runs (no RELAY_BRIDGE_SOCKET set) are a
// clean no-op — direct clients still reach the listener.
//
// Failure is logged and swallowed: the listener is already up, so missing
// the relay-dispatch path is a partial degradation, not a hard error.
func maybeRegisterManifest(internalSocket, internalToken string) {
	if os.Getenv(envBridgeSocket) == "" {
		slog.Info("standalone mode — skipping manifest registration")
		return
	}
	serviceID := os.Getenv(envServiceID)
	if serviceID == "" {
		slog.Warn("bridge socket set but service ID missing — skipping manifest registration", "env", envServiceID)
		return
	}

	manifest := buildManifest()
	args, err := json.Marshal(registerManifestRequest{
		ServiceID:      serviceID,
		Manifest:       manifest,
		InternalSocket: internalSocket,
		InternalToken:  internalToken,
	})
	if err != nil {
		slog.Error("marshal manifest registration failed", "error", err)
		return
	}
	if _, err := sendBridgeRequest(reqRegisterManifest, args); err != nil {
		slog.Error("manifest registration failed; running without relay dispatch", "error", err)
		return
	}
	slog.Info("manifest registered with relay",
		"serviceId", serviceID,
		"internalSocket", internalSocket,
		"routes", len(manifest.Routes))
}
