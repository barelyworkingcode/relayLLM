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
	Routes  []string     `json:"routes"`
	Status  *StatusDecl  `json:"status,omitempty"`
	Actions []ActionDecl `json:"actions,omitempty"`
}

type StatusDecl struct {
	Path string `json:"path"`
}

type ActionDecl struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Method       string `json:"method"`
	PathTemplate string `json:"pathTemplate"`
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
