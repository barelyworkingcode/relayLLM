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
	"strings"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// LlamaModelConfig describes one llama-server model. Alias is the routing
// name (users select "llama/{alias}"). Args holds every other key from the
// JSON entry — each maps 1:1 to a llama-server CLI flag.
type LlamaModelConfig struct {
	Alias string
	Args  map[string]any // key → value, translated to --key [value]
}

// LlamaConfig is the top-level llama_models.json structure.
type LlamaConfig struct {
	BinaryPath string             `json:"binaryPath,omitempty"`
	ModelDir   string             `json:"modelDir,omitempty"` // prepended to relative model paths
	BasePort   int                `json:"basePort,omitempty"`
	Models     []LlamaModelConfig `json:"-"` // custom unmarshal
	RawModels  []map[string]any   `json:"models"`
}

// FindByAlias returns the config for the given alias, or nil.
func (c *LlamaConfig) FindByAlias(alias string) *LlamaModelConfig {
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

// parseLlamaRawModels converts RawModels entries into typed LlamaModelConfig
// values. Each raw entry must have an "alias" key; all other keys become Args.
// If modelDir is set, relative "model" paths are resolved against it.
func parseLlamaRawModels(cfg *LlamaConfig, source string) error {
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
		// Resolve relative model paths against modelDir.
		if modelDir != "" {
			if m, ok := args["model"].(string); ok && !filepath.IsAbs(m) {
				args["model"] = filepath.Join(modelDir, m)
			}
		}
		cfg.Models = append(cfg.Models, LlamaModelConfig{Alias: alias, Args: args})
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

// llamaInstance tracks a running llama-server process.
type llamaInstance struct {
	config  LlamaModelConfig
	port    int
	cmd     *exec.Cmd
	exited  atomic.Bool
	ready   chan struct{} // closed when health check passes (or fails)
	healthy bool         // set before ready is closed
}

// LlamaServerManager launches and manages llama-server processes.
type LlamaServerManager struct {
	mu         sync.Mutex
	config     *LlamaConfig
	binaryPath string
	nextPort   int
	instances  map[string]*llamaInstance // alias → instance
}

// NewLlamaServerManager creates a manager. binaryPathOverride takes priority
// over the config's BinaryPath, which in turn takes priority over "llama-server"
// (PATH lookup).
func NewLlamaServerManager(cfg *LlamaConfig, binaryPathOverride string) *LlamaServerManager {
	bin := "llama-server"
	if cfg.BinaryPath != "" {
		bin = cfg.BinaryPath
	}
	if binaryPathOverride != "" {
		bin = binaryPathOverride
	}

	basePort := cfg.BasePort
	if basePort == 0 {
		basePort = 8090
	}

	return &LlamaServerManager{
		config:     cfg,
		binaryPath: bin,
		nextPort:   basePort,
		instances:  make(map[string]*llamaInstance),
	}
}

// GetOrLaunch returns an OpenAIEndpoint for the given model alias. If a
// server for this model is already running, it reuses it. Otherwise it
// launches a new llama-server process and waits for it to become healthy.
//
// The global mutex is held only for the fast path (reuse) and for process
// startup bookkeeping. The slow health-check poll runs outside the lock so
// concurrent launches of different models proceed in parallel.
func (m *LlamaServerManager) GetOrLaunch(alias string) (*OpenAIEndpoint, error) {
	m.mu.Lock()

	// Fast path: existing instance that hasn't been cleaned up.
	if inst, ok := m.instances[alias]; ok {
		if !inst.exited.Load() {
			m.mu.Unlock()
			// If another goroutine is still launching this model, wait for it.
			<-inst.ready
			if inst.healthy && !inst.exited.Load() {
				return endpointForPort(inst.port), nil
			}
			// Launch failed or process died — fall through to relaunch.
			m.mu.Lock()
			delete(m.instances, alias)
		} else {
			// Stale dead instance — clean up and relaunch.
			delete(m.instances, alias)
		}
	}

	cfg := m.config.FindByAlias(alias)
	if cfg == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("llama: unknown model alias %q", alias)
	}

	binPath, err := exec.LookPath(m.binaryPath)
	if err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("llama: binary %q not found: %w", m.binaryPath, err)
	}

	port := m.portFromArgs(cfg.Args)
	if port == 0 {
		port = m.allocatePort()
	}

	args := buildLlamaArgs(cfg.Args, port)
	slog.Info("llama: launching server", "alias", alias, "binary", binPath, "port", port, "args", args)

	cmd := exec.Command(binPath, args...)
	logProcessOutput(cmd, alias)

	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("llama: failed to start server for %q: %w", alias, err)
	}

	inst := &llamaInstance{
		config: *cfg,
		port:   port,
		cmd:    cmd,
		ready:  make(chan struct{}),
	}
	m.instances[alias] = inst

	// Monitor process exit.
	go func() {
		err := cmd.Wait()
		inst.exited.Store(true)
		if err != nil {
			slog.Warn("llama: server exited", "alias", alias, "port", port, "error", err)
		} else {
			slog.Info("llama: server exited cleanly", "alias", alias, "port", port)
		}
	}()

	// Release the global lock before the slow health check so other models
	// can launch concurrently.
	m.mu.Unlock()

	if err := waitForHealth(port, 120*time.Second); err != nil {
		cmd.Process.Kill()
		m.mu.Lock()
		delete(m.instances, alias)
		m.mu.Unlock()
		close(inst.ready) // unblock any waiters
		return nil, fmt.Errorf("llama: server for %q failed health check: %w", alias, err)
	}

	inst.healthy = true
	close(inst.ready)
	slog.Info("llama: server ready", "alias", alias, "port", port)
	return endpointForPort(port), nil
}

// ListModels returns ModelInfo entries for all configured models.
func (m *LlamaServerManager) ListModels() []ModelInfo {
	models := make([]ModelInfo, len(m.config.Models))
	for i, cfg := range m.config.Models {
		value := "llama/" + cfg.Alias
		models[i] = ModelInfo{
			Label:    value,
			Value:    value,
			Group:    "llama.cpp",
			Provider: "llama",
		}
	}
	return models
}

// StopAll sends SIGTERM to all managed processes, waits briefly, then
// force-kills any stragglers.
func (m *LlamaServerManager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for alias, inst := range m.instances {
		if inst.cmd.Process != nil && !inst.exited.Load() {
			slog.Info("llama: stopping server", "alias", alias, "port", inst.port)
			inst.cmd.Process.Signal(syscall.SIGTERM)
		}
	}

	time.Sleep(3 * time.Second)

	for _, inst := range m.instances {
		if inst.cmd.Process != nil && !inst.exited.Load() {
			inst.cmd.Process.Kill()
		}
	}
	m.instances = make(map[string]*llamaInstance)
}

// allocatePort finds the next free TCP port starting from m.nextPort.
// Must be called with m.mu held.
func (m *LlamaServerManager) allocatePort() int {
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
func (m *LlamaServerManager) portFromArgs(args map[string]any) int {
	if v, ok := args["port"]; ok {
		if f, ok := v.(float64); ok {
			return int(f)
		}
	}
	return 0
}

// buildLlamaArgs translates the Args map into CLI flags. The port and host
// flags are always set (host defaults to 127.0.0.1 if not in the map).
// Keys "port" and "host" in the map are consumed here rather than duplicated.
func buildLlamaArgs(args map[string]any, port int) []string {
	host := "127.0.0.1"
	if h, ok := args["host"].(string); ok {
		host = h
	}
	result := []string{
		"--port", strconv.Itoa(port),
		"--host", host,
	}

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
func (m *LlamaServerManager) Aliases() []string {
	aliases := make([]string, len(m.config.Models))
	for i, cfg := range m.config.Models {
		aliases[i] = cfg.Alias
	}
	return aliases
}

// waitForHealth polls llama-server's /health endpoint until it responds
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

func endpointForPort(port int) *OpenAIEndpoint {
	return &OpenAIEndpoint{
		Name:    "llama",
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d/v1", port),
		Group:   "llama.cpp",
	}
}

// logProcessOutput pipes cmd's stdout and stderr to slog, one line at a
// time via bufio.Scanner. This correctly handles partial writes and
// multi-line output, unlike a bare io.Writer.
func logProcessOutput(cmd *exec.Cmd, alias string) {
	source := fmt.Sprintf("llama[%s]", alias)
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
