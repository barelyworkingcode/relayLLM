package relay

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"

	"relayllm/internal/config"
)

// Manifest is the wire shape relayLLM declares to relay. Must stay
// field-compatible with relay/bridge/manifest.go (which is in a separate
// Go module, so the types are intentionally mirrored here).
type Manifest struct {
	Routes  []string     `json:"routes"`
	Status  *StatusDecl  `json:"status,omitempty"`
	Actions []ActionDecl `json:"actions,omitempty"`
	Config  *ConfigDecl  `json:"config,omitempty"`
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
	// The status payload becomes load-bearing once referenced this way:
	// renaming the array or a row field is a coordinated change across this
	// declaration and relay's UI, not a local rename.
	//
	// Deliberately not built: confirmation prompts on destructive actions
	// (stopping an instance is reversible — relaunch on demand — so this
	// waits until a non-reversible action ships), row-payload validation
	// against a schema (row values pass through opaquely; a JSONSchema
	// declaration is heavier than warranted), and multiple ForEach arrays
	// per action (one action, one array; no cross-array use case yet). Row
	// *values* come from the UI, but the *paths* stay service-declared —
	// relay's dispatcher URL-escapes each substituted value and refuses
	// anything not declared here, so the manifest remains the action
	// whitelist even if the UI were compromised.
	ForEach string `json:"forEach,omitempty"`
}

// ConfigDecl declares relayLLM's editable config file plus the schema relay
// renders an editor from. Mirrors relay/bridge/manifest.go's ConfigDecl.
type ConfigDecl struct {
	Path      string      `json:"path"`
	Format    string      `json:"format,omitempty"`
	Label     string      `json:"label,omitempty"`
	Help      string      `json:"help,omitempty"`
	ApplyMode string      `json:"applyMode,omitempty"`
	Schema    []FieldDecl `json:"schema"`
}

// FieldDecl is one recursive node in a config schema. Mirrors
// relay/bridge/manifest.go's FieldDecl wire shape.
type FieldDecl struct {
	ID          string   `json:"id"`
	Label       string   `json:"label,omitempty"`
	Type        string   `json:"type"`
	Help        string   `json:"help,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Required    bool     `json:"required,omitempty"`
	ReadOnly    bool     `json:"readOnly,omitempty"`
	Secret      bool     `json:"secret,omitempty"`
	Options     []string `json:"options,omitempty"`

	Fields   []FieldDecl `json:"fields,omitempty"`
	Item     *FieldDecl  `json:"item,omitempty"`
	KeyLabel string      `json:"keyLabel,omitempty"`
	Rest     bool        `json:"rest,omitempty"`
}

// RegisterManifestRequest is the Arguments payload for ReqRegisterManifest.
type RegisterManifestRequest struct {
	ServiceID      string   `json:"serviceId"`
	Manifest       Manifest `json:"manifest"`
	InternalSocket string   `json:"internalSocket"`
	InternalToken  string   `json:"internalToken"`
}

// BuildManifest declares the routes, status endpoint, user actions, and the
// editable config file (with a schema relay renders a form from) that relayLLM
// wants relay to dispatch / surface. See ../relay/plans/service-manifest-spec.md.
func BuildManifest(dataDir string) Manifest {
	return Manifest{
		Routes: []string{
			"/api/status",
			"/api/status/detailed",
			"/api/llama/",
			"/api/mlx/",
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
				ID:           "stop-mlx",
				Label:        "Stop",
				Method:       "DELETE",
				PathTemplate: "/api/mlx/instances/{alias}",
				ForEach:      "mlxInstances",
			},
		},
		Config: &ConfigDecl{
			Path:      filepath.Join(dataDir, "settings.json"),
			Format:    "jsonc",
			Label:     "settings.json",
			Help:      "relayLLM configuration. Saving restarts relayLLM to apply.",
			ApplyMode: "restart",
			Schema:    settingsSchema(),
		},
	}
}

// settingsSchema describes the on-disk shape of settings.json so relay can
// render a nested editor. Mirrors the sections parseUnifiedConfig (config.go)
// reads: openai, llama-server, mlx-serve, splash-serve, pi, and the pty terminal-template
// map. Help text replaces the JSONC comments the file used to carry (they
// don't survive a form save).
func settingsSchema() []FieldDecl {
	regen := []string{config.AutoRegenAlways, "skipIfExists", config.AutoRegenNever}
	return []FieldDecl{
		{
			ID: "openai", Label: "OpenAI-compatible endpoints", Type: "object",
			Help: "OpenAI-compatible API providers (LM Studio, Ollama, OpenAI, …).",
			Fields: []FieldDecl{
				{ID: "endpoints", Label: "Endpoints", Type: "array", Item: &FieldDecl{
					Type: "object", Label: "endpoint", Fields: []FieldDecl{
						{ID: "name", Label: "Name", Type: "text", Required: true, Help: `Routing prefix, e.g. "lmstudio".`},
						{ID: "baseURL", Label: "Base URL", Type: "text", Required: true, Placeholder: "http://localhost:1234/v1"},
						{ID: "apiKey", Label: "API key", Type: "secret"},
						{ID: "group", Label: "Group", Type: "text", Help: "Display group; defaults to Name."},
						{ID: "strict", Label: "Strict", Type: "bool", Help: "Gate non-standard fields (OpenAI/Azure)."},
						{ID: "api", Label: "APIs served", Type: "string[]", Help: "Wire APIs this upstream serves: openai, anthropic. Add anthropic to send Claude Code /v1/messages natively instead of translating. Blank = openai."},
					},
				}},
			},
		},
		{
			ID: "virtual-llms", Label: "Virtual LLM failover", Type: "object",
			Help: "Stable model names that route to the first reachable target in order.",
			Fields: []FieldDecl{
				{ID: "models", Label: "Virtual models", Type: "array", Item: &FieldDecl{
					Type: "object", Label: "virtual model", Fields: []FieldDecl{
						{ID: "name", Label: "Name", Type: "text", Required: true, Help: `The model callers use, e.g. "vCode".`},
						{ID: "targets", Label: "Targets", Type: "array", Item: &FieldDecl{Type: "object", Label: "target", Fields: []FieldDecl{
							{ID: "endpoint", Label: "Endpoint", Type: "text", Help: "Name of an OpenAI-compatible endpoint (with Upstream model)."},
							{ID: "model", Label: "Upstream model", Type: "text", Help: "Model id sent to the OpenAI-compatible endpoint."},
							{ID: "alias", Label: "Managed alias", Type: "text", Help: "Local llama.cpp or MLX model alias; use instead of Endpoint/Upstream model."},
							{ID: "params", Label: "Default request fields", Type: "json", Help: "JSON object of top-level request fields sent when the client omits them, e.g. {\"chat_template_kwargs\":{\"enable_thinking\":false}}. Client values win; objects merge key by key. Not checked against the upstream."},
						}}},
					},
				}},
			},
		},
		{
			ID: "llama-server", Label: "llama.cpp server", Type: "object",
			Fields: []FieldDecl{
				{ID: "binaryPath", Label: "Binary path", Type: "text", Placeholder: "/usr/local/bin/llama-server"},
				{ID: "modelDir", Label: "Model directory", Type: "text", Help: "Base directory for relative model paths."},
				{ID: "api", Label: "APIs served", Type: "string[]", Help: "Wire APIs this upstream serves: openai, anthropic. Add anthropic to send Claude Code /v1/messages natively instead of translating. Blank = openai."},
				{ID: "basePort", Label: "Base port", Type: "number", Placeholder: "8090", Help: "First port; each model instance increments from here. Blank uses the 8090 default."},
				{ID: "maxLoaded", Label: "Max loaded models", Type: "number", Help: "Cap on models resident at once. When full, the least-recently-used idle model is stopped. Blank = unlimited."},
				{ID: "maxMemoryGB", Label: "Memory budget (GB)", Type: "number", Help: "Cap on total estimated memory across loaded models. Sizes are computed from each GGUF (weights + KV cache at the configured ctx-size). Blank = unlimited."},
				{ID: "idleTimeoutMinutes", Label: "Idle timeout (minutes)", Type: "number", Help: "Stop a model after this long with nothing using it. Blank = never reclaim."},
				{ID: "memoryHeadroomPercent", Label: "Memory headroom (%)", Type: "number", Placeholder: "10", Help: "Padding added to every estimate for compute buffers, which are not modelled directly. Raise if you run large batch/ubatch sizes."},
				{ID: "admissionTimeoutSeconds", Label: "Admission timeout (s)", Type: "number", Placeholder: "120", Help: "How long a request waits for a busy model to finish when the budget is full, before failing."},
				{ID: "models", Label: "Models", Type: "array", Item: &FieldDecl{
					Type: "object", Label: "model", Fields: []FieldDecl{
						{ID: "alias", Label: "Alias", Type: "text", Required: true, Help: "Routing name (llama/{alias})."},
						{ID: "memoryGB", Label: "Memory override (GB)", Type: "number", Help: "Skip the computed estimate for this model and use this figure. Not a llama-server flag."},
						{ID: "flags", Label: "llama-server flags", Type: "keyValue", Rest: true, KeyLabel: "flag",
							Help: `Each key becomes --key. true/false toggles a boolean flag; numbers and strings pass through as --key value (e.g. model, ctx-size, n-gpu-layers, flash-attn).`},
					},
				}},
			},
		},
		{
			ID: "mlx-serve", Label: "MLX server (mlx-serve)", Type: "object",
			Fields: []FieldDecl{
				{ID: "binaryPath", Label: "Binary path", Type: "text", Placeholder: "~/.local/mlx-serve/mlx-serve"},
				{ID: "modelDir", Label: "Model directory", Type: "text", Help: "Base directory for relative model paths."},
				{ID: "api", Label: "APIs served", Type: "string[]", Help: "Wire APIs this upstream serves: openai, anthropic. Add anthropic to send Claude Code /v1/messages natively instead of translating. Blank = openai."},
				{ID: "basePort", Label: "Base port", Type: "number", Placeholder: "9400", Help: "First port; each model instance increments from here. Blank uses the 9400 default."},
				{ID: "maxLoaded", Label: "Max loaded models", Type: "number", Help: "Cap on models resident at once. When full, the least-recently-used idle model is stopped. Blank = unlimited."},
				{ID: "maxMemoryGB", Label: "Memory budget (GB)", Type: "number", Help: "Cap on total estimated memory across loaded models. Sizes are computed from the model directory (weights + KV cache from config.json). Blank = unlimited."},
				{ID: "idleTimeoutMinutes", Label: "Idle timeout (minutes)", Type: "number", Help: "Stop a model after this long with nothing using it. Blank = never reclaim."},
				{ID: "memoryHeadroomPercent", Label: "Memory headroom (%)", Type: "number", Placeholder: "10", Help: "Padding added to every estimate for compute buffers, which are not modelled directly."},
				{ID: "admissionTimeoutSeconds", Label: "Admission timeout (s)", Type: "number", Placeholder: "120", Help: "How long a request waits for a busy model to finish when the budget is full, before failing."},
				{ID: "models", Label: "Models", Type: "array", Item: &FieldDecl{
					Type: "object", Label: "model", Fields: []FieldDecl{
						{ID: "alias", Label: "Alias", Type: "text", Required: true, Help: "Routing name (mlx/{alias})."},
						{ID: "memoryGB", Label: "Memory override (GB)", Type: "number", Help: "Skip the computed estimate for this model and use this figure. Not an mlx-serve flag."},
						{ID: "flags", Label: "mlx-serve flags", Type: "keyValue", Rest: true, KeyLabel: "flag",
							Help: `Each key becomes --key. model = path to the MLX model DIRECTORY. true/false toggles a boolean flag; numbers and strings pass through as --key value (e.g. ctx-size, temp, kv-quant, max-tokens).`},
					},
				}},
			},
		},
		{
			ID: "splash-serve", Label: "Splash server (splash)", Type: "object",
			Fields: []FieldDecl{
				{ID: "binaryPath", Label: "Binary path", Type: "text", Placeholder: "splash", Help: "Blank uses splash on PATH."},
				{ID: "api", Label: "APIs served", Type: "string[]", Help: "Wire APIs this upstream serves: openai, anthropic. Add anthropic to send Claude Code /v1/messages natively instead of translating. Blank = openai."},
				{ID: "basePort", Label: "Base port", Type: "number", Placeholder: "9500", Help: "First port; each model instance increments from here. Blank uses the 9500 default."},
				{ID: "maxLoaded", Label: "Max loaded models", Type: "number", Help: "Cap on models resident at once. When full, the least-recently-used idle model is stopped. Blank = unlimited."},
				{ID: "maxMemoryGB", Label: "Memory budget (GB)", Type: "number", Help: "Cap on total memory across loaded models. Splash models count only their per-model memoryGB override; a model without one counts against Max loaded but not this budget. Blank = unlimited."},
				{ID: "idleTimeoutMinutes", Label: "Idle timeout (minutes)", Type: "number", Placeholder: "60", Help: "Stop a model after this long with nothing using it. Blank uses the 60 default; 0 = never reclaim."},
				{ID: "memoryHeadroomPercent", Label: "Memory headroom (%)", Type: "number", Placeholder: "10", Help: "Padding added to every estimate."},
				{ID: "admissionTimeoutSeconds", Label: "Admission timeout (s)", Type: "number", Placeholder: "120", Help: "How long a request waits for a busy model to finish when the budget is full, before failing."},
				{ID: "models", Label: "Models", Type: "array", Item: &FieldDecl{
					Type: "object", Label: "model", Fields: []FieldDecl{
						{ID: "alias", Label: "Alias", Type: "text", Required: true, Help: "Routing name (bare alias)."},
						{ID: "memoryGB", Label: "Memory override (GB)", Type: "number", Help: "Resident memory to count for this model. Splash models are sized only by this figure. Not a splash flag."},
						{ID: "flags", Label: "splash flags", Type: "keyValue", Rest: true, KeyLabel: "flag",
							Help: `Each key becomes --key. model = Hugging Face repo id OWNER/REPO or OWNER/REPO:VARIANT; download it first with "splash serve --model <id>". true/false toggles a boolean flag; numbers and strings pass through as --key value (e.g. max-context).`},
					},
				}},
			},
		},
		{
			ID: "pi", Label: "pi.dev coding agent", Type: "object",
			Fields: []FieldDecl{
				{ID: "binaryPath", Label: "Binary path", Type: "text", Placeholder: "/usr/local/bin/pi"},
				{ID: "extraArgs", Label: "Extra args", Type: "string[]", Help: "Appended to every pi spawn."},
				{ID: "useRelayToken", Label: "Use relay token", Type: "bool"},
				{ID: "env_passthrough", Label: "Env passthrough", Type: "string[]", Help: "Env keys forwarded into pi."},
				{ID: "projectOverlay", Label: "Project overlay", Type: "object", Fields: []FieldDecl{
					{ID: "mode", Label: "Mode", Type: "select", Options: regen},
					{ID: "dirName", Label: "Dir name", Type: "text", Placeholder: ".pi"},
					{ID: "authStrategy", Label: "Auth strategy", Type: "select", Options: []string{config.PiAuthStrategySymlink, config.PiAuthStrategyNone}},
					{ID: "defaultProvider", Label: "Default provider", Type: "text"},
					{ID: "defaultModel", Label: "Default model", Type: "text"},
					{ID: "defaultThinking", Label: "Default thinking", Type: "select", Options: []string{"off", "minimal", "low", "medium", "high", "xhigh"}},
					{ID: "extraSkillDirs", Label: "Extra skill dirs", Type: "string[]"},
					{ID: "gitignore", Label: "Gitignore overlay dir", Type: "bool"},
					{ID: "excludeUserProviders", Label: "Exclude user providers", Type: "bool"},
					{ID: "excludeUserSettings", Label: "Exclude user settings", Type: "bool"},
					{ID: "excludeProviders", Label: "Exclude providers", Type: "string[]"},
				}},
			},
		},
		{
			ID: "pty", Label: "Terminal templates", Type: "map", KeyLabel: "template id",
			Help: "Launchable terminal types. Built-ins (claude-code, opencode, shell) live here too — edit with care.",
			Item: &FieldDecl{
				Type: "object", Label: "template", Fields: []FieldDecl{
					{ID: "name", Label: "Name", Type: "text", Required: true},
					{ID: "command", Label: "Command", Type: "text", Help: "Omit for the default shell."},
					{ID: "args", Label: "Args", Type: "string[]"},
					{ID: "env", Label: "Environment", Type: "stringMap"},
					{ID: "icon", Label: "Icon", Type: "text", Placeholder: "terminal"},
					{ID: "description", Label: "Description", Type: "textarea"},
					{ID: "idleTimeout", Label: "Idle timeout (min)", Type: "number"},
					{ID: "useRelayToken", Label: "Use relay token", Type: "bool"},
					{ID: "env_passthrough", Label: "Env passthrough", Type: "string[]"},
				},
			},
		},
	}
}

// MaybeRegisterManifest tells relay where to dispatch front-door traffic
// for this service. A process relay did not launch is a clean no-op — direct
// clients still reach the listener. Returns whether registration actually
// succeeded (false for standalone mode too), so a caller with its own,
// stricter follow-up request — RegisterModelHost (C9) fails closed where
// this one fails open — knows whether it is worth sending at all.
//
// Failure is logged and swallowed: the listener is already up, so missing
// the relay-dispatch path is a partial degradation, not a hard error.
func MaybeRegisterManifest(dataDir, internalSocket, internalToken string) bool {
	if !Launched() {
		slog.Info("standalone mode — skipping manifest registration")
		return false
	}
	serviceID := os.Getenv(EnvServiceID)

	manifest := BuildManifest(dataDir)
	args, err := json.Marshal(RegisterManifestRequest{
		ServiceID:      serviceID,
		Manifest:       manifest,
		InternalSocket: internalSocket,
		InternalToken:  internalToken,
	})
	if err != nil {
		slog.Error("marshal manifest registration failed", "error", err)
		return false
	}
	if _, err := SendBridgeRequest(ReqRegisterManifest, args); err != nil {
		slog.Error("manifest registration failed; running without relay dispatch", "error", err)
		return false
	}
	slog.Info("manifest registered with relay",
		"serviceId", serviceID,
		"internalSocket", internalSocket,
		"routes", len(manifest.Routes))
	return true
}
