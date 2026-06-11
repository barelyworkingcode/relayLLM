package main

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ServerProfile parameterizes ServerManager for a specific managed-server
// binary. Kind doubles as the model routing prefix ("{kind}/{alias}"), the
// ModelInfo.Provider string, and the log/error prefix.
type ServerProfile struct {
	Kind            string   // "llama" | "mlx"
	DefaultBinary   string   // PATH fallback when config/flag give no path
	Group           string   // Eve UI model group label
	FixedArgs       []string // injected after --port/--host, before per-model flags (e.g. --serve)
	DefaultBasePort int
}

var llamaProfile = ServerProfile{Kind: "llama", DefaultBinary: "llama-server", Group: "llama.cpp", DefaultBasePort: 8090}
var mlxProfile = ServerProfile{Kind: "mlx", DefaultBinary: "mlx-serve", Group: "MLX", FixedArgs: []string{"--serve"}, DefaultBasePort: 9400}

// ServerModelConfig describes one managed-server model. Alias is the routing
// name (users select "{kind}/{alias}"). Args holds every other key from the
// JSON entry — each maps 1:1 to a CLI flag.
type ServerModelConfig struct {
	Alias string
	Args  map[string]any // key → value, translated to --key [value]
}

// ServerConfig is the top-level config structure for a managed-server section
// (llama-server or mlx-serve).
type ServerConfig struct {
	BinaryPath string              `json:"binaryPath,omitempty"`
	ModelDir   string              `json:"modelDir,omitempty"` // prepended to relative model paths
	BasePort   int                 `json:"basePort,omitempty"`
	Models     []ServerModelConfig `json:"-"` // custom unmarshal
	RawModels  []map[string]any    `json:"models"`
}

// FindByAlias returns the config for the given alias, or nil.
func (c *ServerConfig) FindByAlias(alias string) *ServerModelConfig {
	if c == nil {
		return nil
	}
	for i := range c.Models {
		if c.Models[i].Alias == alias {
			return &c.Models[i]
		}
	}
	return nil
}

// parseServerRawModels converts RawModels entries into typed ServerModelConfig
// values. Each raw entry must have an "alias" key; all other keys become Args.
// If modelDir is set, relative "model" paths are resolved against it.
func parseServerRawModels(cfg *ServerConfig, source string) error {
	modelDir := expandHome(cfg.ModelDir)

	for i, raw := range cfg.RawModels {
		alias, _ := raw["alias"].(string)
		if alias == "" {
			return fmt.Errorf("parse %s: models[%d] missing \"alias\"", source, i)
		}
		args := make(map[string]any, len(raw)-1)
		for k, v := range raw {
			if k == "alias" {
				continue
			}
			args[k] = v
		}
		// Resolve relative file paths against modelDir.
		if modelDir != "" {
			for _, k := range []string{"model", "mmproj"} {
				if v, ok := args[k].(string); ok && !filepath.IsAbs(v) {
					args[k] = filepath.Join(modelDir, v)
				}
			}
		}
		cfg.Models = append(cfg.Models, ServerModelConfig{Alias: alias, Args: args})
	}
	cfg.RawModels = nil
	return nil
}

// expandHome replaces a leading ~ with the user's home directory.
func expandHome(path string) string {
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "~/") || path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

// serverInstance tracks a running managed-server process.
type serverInstance struct {
	config    ServerModelConfig
	port      int
	cmd       *exec.Cmd
	exited    atomic.Bool
	ready     chan struct{} // closed when health check passes (or fails)
	healthy   atomic.Bool   // set before ready is closed; atomic because ListInstances reads it without waiting on ready
	startTime time.Time
}

// ServerInstanceInfo is a JSON-friendly snapshot of a running managed-server
// instance. Returned by ListInstances() to the relay menubar UI.
type ServerInstanceInfo struct {
	Alias     string `json:"alias"`
	Port      int    `json:"port"`
	Pid       int    `json:"pid"`
	StartedAt string `json:"startedAt"` // RFC3339; UI renders as relative "5m ago"
	Healthy   bool   `json:"healthy"`
	Exited    bool   `json:"exited"`
}

// ServerManager launches and manages managed-server processes (llama-server,
// mlx-serve, …). It is parameterized by a ServerProfile that controls the
// binary name, CLI flag conventions, port range, and log prefix.
type ServerManager struct {
	profile    ServerProfile
	mu         sync.Mutex
	config     *ServerConfig
	binaryPath string
	nextPort   int
	instances  map[string]*serverInstance // alias → instance
}

// NewServerManager creates a manager. binaryPathOverride takes priority over
// the config's BinaryPath, which in turn takes priority over profile.DefaultBinary
// (PATH lookup).
func NewServerManager(profile ServerProfile, cfg *ServerConfig, binaryPathOverride string) *ServerManager {
	bin := profile.DefaultBinary
	if cfg.BinaryPath != "" {
		bin = cfg.BinaryPath
	}
	if binaryPathOverride != "" {
		bin = binaryPathOverride
	}
	// The docs advertise "~/..." paths (e.g. ~/.local/mlx-serve/mlx-serve);
	// exec.LookPath does no tilde expansion, so expand here. A bare binary
	// name (no ~ prefix) passes through unchanged for PATH lookup.
	bin = expandHome(bin)

	basePort := cfg.BasePort
	if basePort == 0 {
		basePort = profile.DefaultBasePort
	}

	return &ServerManager{
		profile:    profile,
		config:     cfg,
		binaryPath: bin,
		nextPort:   basePort,
		instances:  make(map[string]*serverInstance),
	}
}

// GetOrLaunch returns an OpenAIEndpoint for the given model alias. If a
// server for this model is already running, it reuses it. Otherwise it
// launches a new process and waits for it to become healthy.
//
// The global mutex is held only for the fast path (reuse) and for process
// startup bookkeeping. The slow health-check poll runs outside the lock so
// concurrent launches of different models proceed in parallel.
func (m *ServerManager) GetOrLaunch(alias string) (*OpenAIEndpoint, error) {
	m.mu.Lock()

	// Fast path: reuse a live instance, or wait out a launch in progress.
	// Loops because a failed wait can find the map slot already repopulated
	// by another waiter's relaunch — wait on the replacement instead of
	// deleting it and racing a duplicate launch (which would orphan one of
	// the two processes).
	for {
		inst, ok := m.instances[alias]
		if !ok {
			break
		}
		if inst.exited.Load() {
			// Stale dead instance — clean up and relaunch.
			delete(m.instances, alias)
			break
		}
		m.mu.Unlock()
		// If another goroutine is still launching this model, wait for it.
		<-inst.ready
		if inst.healthy.Load() && !inst.exited.Load() {
			return endpointForPort(m.profile, inst.port), nil
		}
		// Launch failed or process died. Relaunch only if the slot still
		// holds the instance we waited on; otherwise re-examine the slot.
		m.mu.Lock()
		if m.instances[alias] == inst {
			delete(m.instances, alias)
			break
		}
	}

	cfg := m.config.FindByAlias(alias)
	if cfg == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("%s: unknown model alias %q", m.profile.Kind, alias)
	}

	binPath, err := exec.LookPath(m.binaryPath)
	if err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("%s: binary %q not found: %w", m.profile.Kind, m.binaryPath, err)
	}

	port := m.portFromArgs(cfg.Args)
	if port == 0 {
		port = m.allocatePort()
	}

	// Pre-bind check: if the port is already held by another process
	// (e.g. a stray server from a previous run, or the user's
	// interactive instance), bail out before spawning. Without this the
	// spawned process EADDRINUSE-crashes silently while waitForHealth
	// happily reports "ready" — it's just talking to the squatter.
	// Only meaningful for user-specified ports; allocatePort already
	// returns a free port but checking again is harmless.
	if err := preflightPortFree(port); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("%s: cannot launch %q: %w", m.profile.Kind, alias, err)
	}

	args := buildServerArgs(m.profile, cfg.Args, port)
	slog.Info(fmt.Sprintf("%s: launching server", m.profile.Kind), "alias", alias, "binary", binPath, "port", port, "args", args)

	cmd := exec.Command(binPath, args...)
	logProcessOutput(cmd, m.profile.Kind, alias)

	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("%s: failed to start server for %q: %w", m.profile.Kind, alias, err)
	}

	inst := &serverInstance{
		config:    *cfg,
		port:      port,
		cmd:       cmd,
		ready:     make(chan struct{}),
		startTime: time.Now(),
	}
	m.instances[alias] = inst

	// Monitor process exit.
	go func() {
		err := cmd.Wait()
		inst.exited.Store(true)
		if err != nil {
			slog.Warn(fmt.Sprintf("%s: server exited", m.profile.Kind), "alias", alias, "port", port, "error", err)
		} else {
			slog.Info(fmt.Sprintf("%s: server exited cleanly", m.profile.Kind), "alias", alias, "port", port)
		}
	}()

	// Release the global lock before the slow health check so other models
	// can launch concurrently.
	m.mu.Unlock()

	if err := waitForHealth(port, 120*time.Second); err != nil {
		cmd.Process.Kill()
		m.removeInstance(alias, inst)
		close(inst.ready) // unblock any waiters
		return nil, fmt.Errorf("%s: server for %q failed health check: %w", m.profile.Kind, alias, err)
	}

	// A 200 from /health proves something is listening on the port — not
	// that it's our child. If our process already exited (e.g. lost a bind
	// race and EADDRINUSE-crashed), the answer came from a squatter.
	if inst.exited.Load() {
		m.removeInstance(alias, inst)
		close(inst.ready)
		return nil, fmt.Errorf("%s: server for %q exited during startup (port %d answered health from another process)", m.profile.Kind, alias, port)
	}

	inst.healthy.Store(true)
	close(inst.ready)
	slog.Info(fmt.Sprintf("%s: server ready", m.profile.Kind), "alias", alias, "port", port)
	return endpointForPort(m.profile, port), nil
}

// removeInstance deletes the alias's map entry only if it still holds inst —
// a concurrent caller may have already replaced a dead entry with its own
// relaunch, which must not be evicted.
func (m *ServerManager) removeInstance(alias string, inst *serverInstance) {
	m.mu.Lock()
	if m.instances[alias] == inst {
		delete(m.instances, alias)
	}
	m.mu.Unlock()
}

// ListModels returns ModelInfo entries for all configured models.
// Attachment support is per-model: present only when the user configured
// an mmproj (multimodal projector) for the underlying server.
func (m *ServerManager) ListModels() []ModelInfo {
	models := make([]ModelInfo, len(m.config.Models))
	for i, cfg := range m.config.Models {
		value := m.profile.Kind + "/" + cfg.Alias
		_, hasMmproj := cfg.Args["mmproj"]
		models[i] = ModelInfo{
			Label:               value,
			Value:               value,
			Group:               m.profile.Group,
			Provider:            m.profile.Kind,
			SupportsAttachments: hasMmproj,
		}
	}
	return models
}

// ListInstances returns a snapshot of all currently-tracked managed-server
// instances. Includes only aliases that have actually been launched, not
// every configured alias.
func (m *ServerManager) ListInstances() []ServerInstanceInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]ServerInstanceInfo, 0, len(m.instances))
	for alias, inst := range m.instances {
		pid := 0
		if inst.cmd != nil && inst.cmd.Process != nil {
			pid = inst.cmd.Process.Pid
		}
		out = append(out, ServerInstanceInfo{
			Alias:     alias,
			Port:      inst.port,
			Pid:       pid,
			StartedAt: inst.startTime.UTC().Format(time.RFC3339),
			Healthy:   inst.healthy.Load(),
			Exited:    inst.exited.Load(),
		})
	}
	return out
}

// StopInstance gracefully terminates a single managed-server (SIGTERM, 3s
// grace, SIGKILL) and removes it from the manager's map. Returns an error
// if the alias is not currently running. The 3s grace sleep happens outside
// the manager lock so other launches/lookups are not blocked.
func (m *ServerManager) StopInstance(alias string) error {
	m.mu.Lock()
	inst, ok := m.instances[alias]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%s: no running instance for alias %q", m.profile.Kind, alias)
	}
	delete(m.instances, alias)
	m.mu.Unlock()

	if inst.cmd == nil || inst.cmd.Process == nil {
		return nil
	}
	if inst.exited.Load() {
		return nil
	}

	slog.Info(fmt.Sprintf("%s: stopping server", m.profile.Kind), "alias", alias, "port", inst.port)
	_ = inst.cmd.Process.Signal(syscall.SIGTERM)

	time.Sleep(3 * time.Second)

	if !inst.exited.Load() {
		_ = inst.cmd.Process.Kill()
	}
	return nil
}

// StopAll terminates every managed process concurrently. Each alias is
// stopped via StopInstance so the SIGTERM/grace/SIGKILL logic lives in one
// place; running them in parallel keeps total shutdown bounded by the 3s
// grace regardless of how many instances are alive.
func (m *ServerManager) StopAll() {
	m.mu.Lock()
	aliases := make([]string, 0, len(m.instances))
	for alias := range m.instances {
		aliases = append(aliases, alias)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, alias := range aliases {
		wg.Add(1)
		go func(a string) {
			defer wg.Done()
			_ = m.StopInstance(a)
		}(alias)
	}
	wg.Wait()
}

// allocatePort finds the next free TCP port starting from m.nextPort.
// Must be called with m.mu held.
func (m *ServerManager) allocatePort() int {
	for {
		port := m.nextPort
		m.nextPort++
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue // port occupied, try next
		}
		ln.Close()
		return port
	}
}

// portFromArgs extracts an explicit port from the args map, or returns 0
// if none is set. Must be called with m.mu held.
func (m *ServerManager) portFromArgs(args map[string]any) int {
	if v, ok := args["port"]; ok {
		if f, ok := v.(float64); ok {
			return int(f)
		}
	}
	return 0
}

// buildServerArgs translates the Args map into CLI flags. The port and host
// flags are always set (host defaults to 127.0.0.1 if not in the map).
// profile.FixedArgs are injected right after --port/--host, before the
// sorted map flags. Keys "port" and "host" in the map are consumed here
// rather than duplicated.
func buildServerArgs(profile ServerProfile, args map[string]any, port int) []string {
	host := "127.0.0.1"
	if h, ok := args["host"].(string); ok {
		host = h
	}
	result := []string{
		"--port", strconv.Itoa(port),
		"--host", host,
	}

	// Inject profile-specific fixed args (e.g. --serve for mlx-serve).
	result = append(result, profile.FixedArgs...)

	// Sort keys for deterministic arg order (easier to debug in logs).
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		// port and host are handled above.
		if key == "port" || key == "host" {
			continue
		}
		val := args[key]
		flag := "--" + key
		switch v := val.(type) {
		case bool:
			if v {
				result = append(result, flag)
			}
			// false → omit
		case float64:
			// JSON numbers are float64. Use integer format if whole number.
			if v == float64(int64(v)) {
				result = append(result, flag, strconv.FormatInt(int64(v), 10))
			} else {
				result = append(result, flag, strconv.FormatFloat(v, 'f', -1, 64))
			}
		case string:
			result = append(result, flag, v)
		default:
			// Fallback: stringify via fmt.
			result = append(result, flag, fmt.Sprintf("%v", v))
		}
	}
	return result
}

// Aliases returns the alias names of all configured models.
func (m *ServerManager) Aliases() []string {
	aliases := make([]string, len(m.config.Models))
	for i, cfg := range m.config.Models {
		aliases[i] = cfg.Alias
	}
	return aliases
}

// HasAlias reports whether the given name matches a configured managed-server
// alias. The router's request dispatch uses this to pick the managed branch
// before falling through to the OpenAI-endpoint branch.
func (m *ServerManager) HasAlias(name string) bool {
	if m == nil || m.config == nil {
		return false
	}
	for _, cfg := range m.config.Models {
		if cfg.Alias == name {
			return true
		}
	}
	return false
}

// preflightPortFree returns an error if some other process is already
// listening on port. The bind+close races against any concurrent launcher
// on the same machine, but the window is microseconds — good enough for
// catching the "stale server squatting on the port" failure mode.
func preflightPortFree(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("port %d already in use (likely a stale server from a previous run; try `lsof -i :%d` to find it)", port, port)
	}
	_ = ln.Close()
	return nil
}

// waitForHealth polls the managed-server's /health endpoint until it responds
// with status 200, or the timeout expires.
func waitForHealth(port int, timeout time.Duration) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("server at port %d did not become healthy within %s", port, timeout)
}

func endpointForPort(profile ServerProfile, port int) *OpenAIEndpoint {
	return &OpenAIEndpoint{
		Name:    profile.Kind,
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d/v1", port),
		Group:   profile.Group,
	}
}

// logProcessOutput pipes cmd's stdout and stderr to slog, one line at a
// time via bufio.Scanner. This correctly handles partial writes and
// multi-line output, unlike a bare io.Writer.
func logProcessOutput(cmd *exec.Cmd, kind, alias string) {
	source := fmt.Sprintf("%s[%s]", kind, alias)
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		go func() {
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				slog.Debug(scanner.Text(), "source", source)
			}
		}()
	}
	stderr, err := cmd.StderrPipe()
	if err == nil {
		go func() {
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				slog.Warn(scanner.Text(), "source", source)
			}
		}()
	}
}
