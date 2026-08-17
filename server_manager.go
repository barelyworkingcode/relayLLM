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

	// Resource budget. All optional; zero means "no limit" for the caps and
	// "no reclaim" for the idle timeout, which is the pre-budget behavior.
	//
	// MaxLoaded caps instance count; MaxMemoryGB caps the sum of estimated
	// resident memory (see server_memory.go). Either can trigger eviction —
	// the count cap is exact but blunt, the memory cap tracks the fact that
	// two loaded models can differ by 6x. Models whose size cannot be
	// estimated count toward MaxLoaded but not MaxMemoryGB.
	MaxLoaded          int     `json:"maxLoaded,omitempty"`
	MaxMemoryGB        float64 `json:"maxMemoryGB,omitempty"`
	IdleTimeoutMinutes int     `json:"idleTimeoutMinutes,omitempty"`

	// MemoryHeadroomPercent pads each model's estimate to cover compute
	// buffers and allocator slack, which are not modelled directly.
	// Defaults to defaultMemoryHeadroomPercent.
	MemoryHeadroomPercent int `json:"memoryHeadroomPercent,omitempty"`

	// AdmissionTimeoutSeconds bounds how long a request waits for a busy
	// instance to go idle when the budget is full. Defaults to
	// defaultAdmissionTimeout.
	AdmissionTimeoutSeconds int `json:"admissionTimeoutSeconds,omitempty"`
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

	// Budget accounting, all guarded by ServerManager.mu.
	//
	// leases counts in-flight users. An instance with leases > 0 is mid-turn
	// and must never be evicted; the idle reaper and the LRU victim search
	// both skip it. lastUsed is stamped on acquire and on release, so an
	// instance that streams for an hour is not considered idle for that hour.
	leases   int
	lastUsed time.Time
	memory   int64 // estimated resident bytes; 0 when unknown
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

	// Budget fields. Leases > 0 means the instance is serving a turn right
	// now and is not eligible for eviction. EstimatedBytes is 0 when the
	// model's size could not be determined.
	Leases         int    `json:"leases"`
	EstimatedBytes int64  `json:"estimatedBytes"`
	EstimatedGB    string `json:"estimatedGB"`
	IdleSeconds    int    `json:"idleSeconds"` // 0 while leased
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

	clock Clock

	// Budget, resolved from config at construction.
	maxLoaded        int
	maxMemoryBytes   int64
	idleTimeout      time.Duration
	admissionTimeout time.Duration

	// memory holds the pre-computed estimate per configured alias so
	// admission control never touches the filesystem while holding mu.
	memory map[string]int64

	// idleSignal is closed and replaced every time an instance releases its
	// last lease or is removed. A goroutine blocked on admission grabs the
	// current channel under mu, then selects on it — a broadcast that works
	// with a timeout, which sync.Cond does not.
	idleSignal chan struct{}

	reaperStop chan struct{}
	reaperOnce sync.Once
}

// defaultAdmissionTimeout bounds the wait for a busy instance to go idle
// before the request is rejected.
const defaultAdmissionTimeout = 120 * time.Second

// idleReapInterval is how often the reaper scans for instances past their
// idle timeout. Coarse on purpose: reclaiming 20GB thirty seconds late costs
// nothing, and a tight loop would just burn wakeups.
const idleReapInterval = 30 * time.Second

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

	m := &ServerManager{
		profile:    profile,
		config:     cfg,
		binaryPath: bin,
		nextPort:   basePort,
		instances:  make(map[string]*serverInstance),
		clock:      DefaultClock,
		memory:     make(map[string]int64, len(cfg.Models)),
		idleSignal: make(chan struct{}),
		reaperStop: make(chan struct{}),

		maxLoaded:        cfg.MaxLoaded,
		maxMemoryBytes:   int64(cfg.MaxMemoryGB * bytesPerGB),
		idleTimeout:      time.Duration(cfg.IdleTimeoutMinutes) * time.Minute,
		admissionTimeout: time.Duration(cfg.AdmissionTimeoutSeconds) * time.Second,
	}
	if m.admissionTimeout <= 0 {
		m.admissionTimeout = defaultAdmissionTimeout
	}

	// Size every configured model up front. Reading GGUF headers is cheap
	// (metadata only) and doing it here keeps the filesystem out of the
	// admission path, which runs under the manager lock.
	for _, mc := range cfg.Models {
		est := estimateModelMemory(profile, mc, cfg.MemoryHeadroomPercent)
		m.memory[mc.Alias] = est
		slog.Debug("memory estimate", "kind", profile.Kind, "alias", mc.Alias, "estimated", formatGB(est))
	}
	if m.maxLoaded > 0 || m.maxMemoryBytes > 0 || m.idleTimeout > 0 {
		slog.Info("managed-server budget", "kind", profile.Kind,
			"maxLoaded", m.maxLoaded, "maxMemory", formatGB(m.maxMemoryBytes),
			"idleTimeout", m.idleTimeout)
	}
	return m
}

// GetOrLaunch returns an OpenAIEndpoint for the given model alias, holding no
// lease: the instance may be evicted the moment this returns. Callers that
// will actually send traffic should use Acquire and hold the lease for the
// duration of the work.
func (m *ServerManager) GetOrLaunch(alias string) (*OpenAIEndpoint, error) {
	endpoint, release, err := m.Acquire(alias)
	if err != nil {
		return nil, err
	}
	release()
	return endpoint, nil
}

// Acquire returns an endpoint for the given alias plus a release function the
// caller must invoke when done. The instance will not be evicted while any
// lease is outstanding, and its idle timer only starts once the last lease is
// released. release is idempotent.
//
// If a server for this alias is already running it is reused. Otherwise the
// budget is checked first: when the manager is at its instance or memory cap,
// the least-recently-used *idle* instance is stopped to make room. If every
// loaded instance is busy, Acquire waits for one to go idle, bounded by the
// admission timeout.
func (m *ServerManager) Acquire(alias string) (*OpenAIEndpoint, func(), error) {
	if m.config.FindByAlias(alias) == nil {
		return nil, nil, fmt.Errorf("%s: unknown model alias %q", m.profile.Kind, alias)
	}

	need := m.memory[alias]
	// A model that cannot fit even in an empty manager will never be admitted;
	// say so now rather than after a two-minute wait for an eviction that
	// cannot help.
	if m.maxMemoryBytes > 0 && need > m.maxMemoryBytes {
		return nil, nil, fmt.Errorf("%s: model %q needs ~%s but the budget is %s (raise maxMemoryGB, lower its ctx-size, or set memoryGB)",
			m.profile.Kind, alias, formatGB(need), formatGB(m.maxMemoryBytes))
	}

	deadline := m.clock.Now().Add(m.admissionTimeout)

	for {
		m.mu.Lock()

		// Fast path: reuse a live instance, or wait out a launch in progress.
		// Loops because a failed wait can find the map slot already repopulated
		// by another waiter's relaunch — wait on the replacement instead of
		// deleting it and racing a duplicate launch (which would orphan one of
		// the two processes).
		if inst, ok := m.instances[alias]; ok {
			switch {
			case inst.exited.Load():
				// Stale dead instance — drop it and fall through to relaunch
				// on the next pass.
				m.dropLocked(alias, inst)
				m.mu.Unlock()
			case inst.healthy.Load():
				m.leaseLocked(inst)
				port := inst.port
				m.mu.Unlock()
				return endpointForPort(m.profile, port), m.releaser(inst), nil
			default:
				// Another goroutine is still launching this model.
				m.mu.Unlock()
				select {
				case <-inst.ready:
				case <-m.clock.After(m.timeUntil(deadline)):
				}
				// If the launch failed, drop the instance so the next pass
				// relaunches — but only if the slot still holds it.
				if !inst.healthy.Load() || inst.exited.Load() {
					m.mu.Lock()
					m.dropLocked(alias, inst)
					m.mu.Unlock()
				}
			}
			if err := m.checkDeadline(alias, deadline); err != nil {
				return nil, nil, err
			}
			continue
		}

		// Admission control. Both the budget check and the launch happen in
		// this one critical section, so two concurrent Acquires for different
		// aliases cannot both observe room and both spend it.
		if !m.fitsLocked(alias, need) {
			victim, victimIdle := m.lruIdleVictimLocked(alias)
			wait := m.idleSignal
			m.mu.Unlock()

			if victimIdle {
				slog.Info(fmt.Sprintf("%s: evicting idle instance to make room", m.profile.Kind),
					"evicting", victim, "for", alias, "needs", formatGB(need))
				_ = m.StopInstance(victim)
			} else {
				// Everything loaded is mid-turn. Wait for a release rather
				// than killing someone's in-flight generation.
				slog.Info(fmt.Sprintf("%s: budget full, waiting for an instance to go idle", m.profile.Kind),
					"alias", alias, "needs", formatGB(need))
				select {
				case <-wait:
				case <-m.clock.After(m.timeUntil(deadline)):
				}
			}
			if err := m.checkDeadline(alias, deadline); err != nil {
				return nil, nil, err
			}
			continue
		}

		inst, err := m.launchLocked(alias, need)
		if err != nil {
			m.mu.Unlock()
			return nil, nil, err
		}
		// Hold a lease across the health check so a concurrent Acquire cannot
		// evict a server that is still starting up.
		m.leaseLocked(inst)
		m.mu.Unlock()

		release := m.releaser(inst)
		if err := m.awaitReady(alias, inst); err != nil {
			release()
			return nil, nil, err
		}
		return endpointForPort(m.profile, inst.port), release, nil
	}
}

// checkDeadline converts an expired admission deadline into a user-facing
// error naming the instances that were holding the budget.
func (m *ServerManager) checkDeadline(alias string, deadline time.Time) error {
	if m.clock.Now().Before(deadline) {
		return nil
	}
	m.mu.Lock()
	busy := make([]string, 0, len(m.instances))
	for a, inst := range m.instances {
		if inst.leases > 0 {
			busy = append(busy, a)
		}
	}
	m.mu.Unlock()
	sort.Strings(busy)
	if len(busy) > 0 {
		return fmt.Errorf("%s: timed out waiting for capacity to run %q; busy: %s",
			m.profile.Kind, alias, strings.Join(busy, ", "))
	}
	return fmt.Errorf("%s: timed out waiting for capacity to run %q", m.profile.Kind, alias)
}

// timeUntil returns the remaining time before deadline, floored at zero so a
// Clock.After call on an expired deadline fires immediately.
func (m *ServerManager) timeUntil(deadline time.Time) time.Duration {
	d := deadline.Sub(m.clock.Now())
	if d < 0 {
		return 0
	}
	return d
}

// launchLocked starts a server process for alias and records the instance.
// Must be called with m.mu held and with the budget already checked; the
// caller keeps the lock until the instance is in the map so the budget cannot
// be double-spent. cmd.Start is a fork/exec, fast enough to hold the lock for.
func (m *ServerManager) launchLocked(alias string, memory int64) (*serverInstance, error) {
	cfg := m.config.FindByAlias(alias)
	if cfg == nil {
		return nil, fmt.Errorf("%s: unknown model alias %q", m.profile.Kind, alias)
	}

	binPath, err := exec.LookPath(m.binaryPath)
	if err != nil {
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
		return nil, fmt.Errorf("%s: cannot launch %q: %w", m.profile.Kind, alias, err)
	}

	args := buildServerArgs(m.profile, cfg.Args, port)
	slog.Info(fmt.Sprintf("%s: launching server", m.profile.Kind), "alias", alias,
		"binary", binPath, "port", port, "estimated", formatGB(memory), "args", args)

	cmd := exec.Command(binPath, args...)
	logProcessOutput(cmd, m.profile.Kind, alias)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: failed to start server for %q: %w", m.profile.Kind, alias, err)
	}

	inst := &serverInstance{
		config:    *cfg,
		port:      port,
		cmd:       cmd,
		ready:     make(chan struct{}),
		startTime: time.Now(),
		lastUsed:  m.clock.Now(),
		memory:    memory,
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

	return inst, nil
}

// awaitReady polls the new instance's health endpoint and publishes the
// result. Runs outside the manager lock so concurrent launches of different
// models proceed in parallel.
func (m *ServerManager) awaitReady(alias string, inst *serverInstance) error {
	if err := waitForHealth(inst.port, 120*time.Second); err != nil {
		inst.cmd.Process.Kill()
		m.removeInstance(alias, inst)
		close(inst.ready) // unblock any waiters
		return fmt.Errorf("%s: server for %q failed health check: %w", m.profile.Kind, alias, err)
	}

	// A 200 from /health proves something is listening on the port — not
	// that it's our child. If our process already exited (e.g. lost a bind
	// race and EADDRINUSE-crashed), the answer came from a squatter.
	if inst.exited.Load() {
		m.removeInstance(alias, inst)
		close(inst.ready)
		return fmt.Errorf("%s: server for %q exited during startup (port %d answered health from another process)",
			m.profile.Kind, alias, inst.port)
	}

	inst.healthy.Store(true)
	close(inst.ready)
	slog.Info(fmt.Sprintf("%s: server ready", m.profile.Kind), "alias", alias, "port", inst.port)
	return nil
}

// fitsLocked reports whether launching alias would stay inside the budget.
// The alias's own existing instance (if any) is excluded from the totals since
// it is about to be replaced. Models with an unknown size (need == 0) are
// never blocked by the memory cap, only by the instance cap.
func (m *ServerManager) fitsLocked(alias string, need int64) bool {
	var (
		count int
		used  int64
	)
	for a, inst := range m.instances {
		if a == alias || inst.exited.Load() {
			continue
		}
		count++
		used += inst.memory
	}
	if m.maxLoaded > 0 && count+1 > m.maxLoaded {
		return false
	}
	if m.maxMemoryBytes > 0 && need > 0 && used+need > m.maxMemoryBytes {
		return false
	}
	return true
}

// lruIdleVictimLocked picks the least-recently-used instance with no
// outstanding leases. Leased instances are never candidates — evicting one
// would kill an in-flight generation.
func (m *ServerManager) lruIdleVictimLocked(exclude string) (string, bool) {
	var (
		victim string
		oldest time.Time
	)
	for a, inst := range m.instances {
		if a == exclude || inst.leases > 0 || inst.exited.Load() {
			continue
		}
		if victim == "" || inst.lastUsed.Before(oldest) {
			victim, oldest = a, inst.lastUsed
		}
	}
	return victim, victim != ""
}

// leaseLocked registers a new user of inst. Must be called with m.mu held.
func (m *ServerManager) leaseLocked(inst *serverInstance) {
	inst.leases++
	inst.lastUsed = m.clock.Now()
}

// releaser returns an idempotent release function for inst.
func (m *ServerManager) releaser(inst *serverInstance) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			if inst.leases > 0 {
				inst.leases--
			}
			inst.lastUsed = m.clock.Now()
			if inst.leases == 0 {
				m.signalIdleLocked()
			}
			m.mu.Unlock()
		})
	}
}

// signalIdleLocked wakes every goroutine waiting for capacity. Closing and
// replacing the channel is a broadcast that composes with a timeout, which is
// why this is not a sync.Cond. Must be called with m.mu held.
func (m *ServerManager) signalIdleLocked() {
	close(m.idleSignal)
	m.idleSignal = make(chan struct{})
}

// dropLocked removes inst from the alias slot if it is still the current
// occupant, and wakes admission waiters since capacity just freed up.
// Must be called with m.mu held.
func (m *ServerManager) dropLocked(alias string, inst *serverInstance) {
	if m.instances[alias] != inst {
		return
	}
	delete(m.instances, alias)
	m.signalIdleLocked()
}

// removeInstance deletes the alias's map entry only if it still holds inst —
// a concurrent caller may have already replaced a dead entry with its own
// relaunch, which must not be evicted.
func (m *ServerManager) removeInstance(alias string, inst *serverInstance) {
	m.mu.Lock()
	m.dropLocked(alias, inst)
	m.mu.Unlock()
}

// StartIdleReaper runs a background sweep that stops instances which have had
// no leases for longer than the configured idle timeout. No-op when the idle
// timeout is unset. Safe to call once; subsequent calls do nothing.
func (m *ServerManager) StartIdleReaper() {
	if m == nil || m.idleTimeout <= 0 {
		return
	}
	m.reaperOnce.Do(func() {
		go func() {
			for {
				select {
				case <-m.reaperStop:
					return
				case <-m.clock.After(idleReapInterval):
					m.ReapIdle()
				}
			}
		}()
	})
}

// ReapIdle stops every instance whose idle time exceeds the configured
// timeout. Exported so tests can drive a sweep directly instead of waiting on
// the reaper's ticker.
func (m *ServerManager) ReapIdle() {
	if m.idleTimeout <= 0 {
		return
	}
	m.mu.Lock()
	var victims []string
	for alias, inst := range m.instances {
		if inst.leases == 0 && !inst.exited.Load() && m.clock.Since(inst.lastUsed) >= m.idleTimeout {
			victims = append(victims, alias)
		}
	}
	m.mu.Unlock()

	sort.Strings(victims)
	for _, alias := range victims {
		slog.Info(fmt.Sprintf("%s: reclaiming idle instance", m.profile.Kind),
			"alias", alias, "idleTimeout", m.idleTimeout)
		_ = m.StopInstance(alias)
	}
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
		idle := 0
		if inst.leases == 0 {
			if d := m.clock.Since(inst.lastUsed); d > 0 {
				idle = int(d.Seconds())
			}
		}
		out = append(out, ServerInstanceInfo{
			Alias:          alias,
			Port:           inst.port,
			Pid:            pid,
			StartedAt:      inst.startTime.UTC().Format(time.RFC3339),
			Healthy:        inst.healthy.Load(),
			Exited:         inst.exited.Load(),
			Leases:         inst.leases,
			EstimatedBytes: inst.memory,
			EstimatedGB:    formatGB(inst.memory),
			IdleSeconds:    idle,
		})
	}
	return out
}

// BudgetInfo summarizes a manager's configured caps and current usage.
// Surfaced through /api/status so the Service Inspector can show why a model
// is or is not loaded.
type BudgetInfo struct {
	Kind         string  `json:"kind"`
	MaxLoaded    int     `json:"maxLoaded"`
	Loaded       int     `json:"loaded"`
	MaxMemoryGB  float64 `json:"maxMemoryGB"`
	UsedMemoryGB float64 `json:"usedMemoryGB"`
	IdleTimeout  string  `json:"idleTimeout,omitempty"`
}

// Budget returns the manager's current budget usage.
func (m *ServerManager) Budget() BudgetInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	var used int64
	loaded := 0
	for _, inst := range m.instances {
		if inst.exited.Load() {
			continue
		}
		loaded++
		used += inst.memory
	}
	info := BudgetInfo{
		Kind:         m.profile.Kind,
		MaxLoaded:    m.maxLoaded,
		Loaded:       loaded,
		MaxMemoryGB:  float64(m.maxMemoryBytes) / bytesPerGB,
		UsedMemoryGB: float64(used) / bytesPerGB,
	}
	if m.idleTimeout > 0 {
		info.IdleTimeout = m.idleTimeout.String()
	}
	return info
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
	// Freeing the slot releases budget, so wake anyone waiting on admission.
	m.dropLocked(alias, inst)
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
	m.reaperOnce.Do(func() {}) // ensure a later StartIdleReaper cannot resurrect the loop
	select {
	case <-m.reaperStop: // already closed
	default:
		close(m.reaperStop)
	}

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
		// port and host are handled above. memoryGB is a relayLLM budget
		// override, not a server flag — passing it through would hand
		// llama-server an unknown --memoryGB and abort the launch.
		if key == "port" || key == "host" || key == "memoryGB" {
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
