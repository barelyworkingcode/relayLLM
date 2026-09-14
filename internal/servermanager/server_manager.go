package servermanager

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	clk "relayllm/internal/clock"
	"relayllm/internal/config"
	"relayllm/internal/spawn"
	"relayllm/internal/types"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var LlamaProfile = config.ServerProfile{Kind: "llama", DefaultBinary: "llama-server", Group: "llama.cpp", DefaultBasePort: 8090}

// mlx-serve (ddalcu/mlx-serve, Zig + mlx-c, zero Python) was chosen over two
// alternatives for the MLX profile: mlx_lm.server (Apple's own) needs a
// pip/uv-managed Python environment, and SwiftLM has no Homebrew
// distribution — its formula builds from source and pins a minimum Xcode a
// beta-OS machine may not satisfy. mlx-serve ships pre-built arm64 release
// tarballs as well as a brew tap, sidestepping that. It speaks the same
// OpenAI-compatible /v1/chat/completions + SSE + GET /health shape as
// llama-server and takes local MLX model directories, which is what let
// ServerManager generalize to both binaries via one config.ServerProfile instead of
// a second ~500-line manager.
var MlxProfile = config.ServerProfile{Kind: "mlx", DefaultBinary: "mlx-serve", Group: "MLX", FixedArgs: []string{"--serve"}, DefaultBasePort: 9400}

// serverInstance tracks a running managed-server process.
type serverInstance struct {
	config    config.ServerModelConfig
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
// mlx-serve, …). It is parameterized by a config.ServerProfile that controls the
// binary name, CLI flag conventions, port range, and log prefix.
type ServerManager struct {
	profile    config.ServerProfile
	mu         sync.Mutex
	config     *config.ServerConfig
	binaryPath string
	nextPort   int
	instances  map[string]*serverInstance // alias → instance

	clock clk.Clock

	// Budget, resolved from config at construction.
	maxLoaded        int
	maxMemoryBytes   int64
	idleTimeout      time.Duration
	admissionTimeout time.Duration

	// memory holds the pre-computed estimate per configured alias so
	// admission control never touches the filesystem while holding mu.
	memory map[string]int64

	// trainedContext holds each model's native context length, read from
	// metadata at construction so catalog listings never touch the filesystem.
	trainedContext map[string]int64

	// loadErrors records why the last explicit StartLoad failed, per alias.
	// Without it a client polling for "loaded" would spin forever on a model
	// that can never start.
	loadErrors map[string]string

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
func NewServerManager(profile config.ServerProfile, cfg *config.ServerConfig, binaryPathOverride string) *ServerManager {
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
	bin = config.ExpandHome(bin)

	basePort := cfg.BasePort
	if basePort == 0 {
		basePort = profile.DefaultBasePort
	}

	m := &ServerManager{
		profile:        profile,
		config:         cfg,
		binaryPath:     bin,
		nextPort:       basePort,
		instances:      make(map[string]*serverInstance),
		clock:          clk.DefaultClock,
		memory:         make(map[string]int64, len(cfg.Models)),
		trainedContext: make(map[string]int64, len(cfg.Models)),
		loadErrors:     make(map[string]string),
		idleSignal:     make(chan struct{}),
		reaperStop:     make(chan struct{}),

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
		m.trainedContext[mc.Alias] = modelTrainedContext(profile, mc)
		slog.Debug("memory estimate", "kind", profile.Kind, "alias", mc.Alias, "estimated", formatGB(est))
	}
	if m.maxLoaded > 0 || m.maxMemoryBytes > 0 || m.idleTimeout > 0 {
		slog.Info("managed-server budget", "kind", profile.Kind,
			"maxLoaded", m.maxLoaded, "maxMemory", formatGB(m.maxMemoryBytes),
			"idleTimeout", m.idleTimeout)
	}
	return m
}

// Profile returns the manager's server profile (llama-server, mlx-serve, …).
func (m *ServerManager) Profile() config.ServerProfile {
	return m.profile
}

// SetClock overrides the manager's clock. Test-only seam for driving the
// idle reaper / admission timeout deterministically with a testutil.FakeClock.
func (m *ServerManager) SetClock(c clk.Clock) {
	m.clock = c
}

// Config returns the manager's underlying model configuration.
func (m *ServerManager) Config() *config.ServerConfig {
	return m.config
}

// BinaryPath returns the resolved path (or PATH-relative name) of the
// managed-server binary this manager launches.
func (m *ServerManager) BinaryPath() string {
	return m.binaryPath
}

// InjectReadyInstanceForTest fabricates a healthy, ready instance for alias
// as if a launch had already completed on port, and registers it in the
// manager. memoryBytes overrides the pre-computed estimate for this instance
// when non-zero. Test-only seam: production code never needs to fabricate an
// instance, only launchLocked does.
func (m *ServerManager) InjectReadyInstanceForTest(alias string, port int, memoryBytes int64) {
	if memoryBytes == 0 {
		memoryBytes = m.memory[alias]
	}
	inst := &serverInstance{
		config:    config.ServerModelConfig{Alias: alias},
		port:      port,
		ready:     make(chan struct{}),
		startTime: m.clock.Now(),
		lastUsed:  m.clock.Now(),
		memory:    memoryBytes,
	}
	inst.healthy.Store(true)
	close(inst.ready)

	m.mu.Lock()
	m.instances[alias] = inst
	m.mu.Unlock()
}

// SetTrainedContextForTest overrides the cached native-context value for
// alias, as if it had been read from the model's own metadata at
// construction. Test-only seam for catalog metadata coverage without a real
// GGUF/MLX config.json on disk.
func (m *ServerManager) SetTrainedContextForTest(alias string, ctx int64) {
	m.mu.Lock()
	m.trainedContext[alias] = ctx
	m.mu.Unlock()
}

// SetLoadErrorForTest records a fake StartLoad failure for alias, as if an
// explicit load had just failed. Test-only seam for exercising ModelCatalog's
// failed/error surfacing without driving a real failing launch.
func (m *ServerManager) SetLoadErrorForTest(alias, msg string) {
	m.mu.Lock()
	m.loadErrors[alias] = msg
	m.mu.Unlock()
}

// LoadErrorForTest returns the currently recorded load error for alias, if
// any. Test-only seam.
func (m *ServerManager) LoadErrorForTest(alias string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadErrors[alias]
}

// InjectInstanceForTest fabricates a healthy running instance for alias with
// the given lease count and last-used time, and registers it in the manager.
// leases > 0 marks it busy/mid-generation, exempt from idle eviction — used
// to test admission control and the idle reaper without a real process.
func (m *ServerManager) InjectInstanceForTest(alias string, leases int, lastUsed time.Time) {
	inst := &serverInstance{
		config:    config.ServerModelConfig{Alias: alias},
		port:      9000,
		ready:     make(chan struct{}),
		startTime: lastUsed,
		lastUsed:  lastUsed,
		leases:    leases,
		memory:    m.memory[alias],
	}
	inst.healthy.Store(true)
	close(inst.ready)

	m.mu.Lock()
	m.instances[alias] = inst
	m.mu.Unlock()
}

// InjectLoadingInstanceForTest registers a not-yet-healthy instance for
// alias, as ModelCatalog reports mid-launch (status "loading"). The returned
// func marks it healthy, simulating the health check completing. Test-only
// seam: production code reaches this state through launchLocked/awaitReady,
// never by direct construction.
func (m *ServerManager) InjectLoadingInstanceForTest(alias string) func() {
	inst := &serverInstance{ready: make(chan struct{}), lastUsed: m.clock.Now()}
	m.mu.Lock()
	m.instances[alias] = inst
	m.mu.Unlock()
	return func() { inst.healthy.Store(true) }
}

// GetOrLaunch returns a config.OpenAIEndpoint for the given model alias, holding no
// lease: the instance may be evicted the moment this returns. Callers that
// will actually send traffic should use Acquire and hold the lease for the
// duration of the work.
func (m *ServerManager) GetOrLaunch(ctx context.Context, alias string) (*config.OpenAIEndpoint, error) {
	endpoint, release, err := m.Acquire(ctx, alias)
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
//
// ctx bounds this caller's *waiting* — for another goroutine's in-progress
// launch of the same alias, or for the budget to free up — so a caller that
// disconnects mid-wait doesn't keep spending admission time, and doesn't
// evict an idle instance for a response nobody is waiting on. It does not
// bound a launch this call itself owns: once launchLocked has started the
// process, this goroutine rides out the health check to completion
// regardless of ctx (see the comment at the awaitReady call below for why
// that one is not cancellable).
func (m *ServerManager) Acquire(ctx context.Context, alias string) (*config.OpenAIEndpoint, func(), error) {
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
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("%s: %q: %w", m.profile.Kind, alias, err)
		}

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
				readyClosed := false
				select {
				case <-inst.ready:
					readyClosed = true
				case <-ctx.Done():
				case <-m.clock.After(m.timeUntil(deadline)):
				}
				// Only drop the instance once the launch itself has actually
				// finished and failed (ready closed with healthy still
				// false, or the process exited — the two are always paired,
				// see launchLocked/awaitReady). A bare wait timeout or
				// cancellation means the launch may still be in progress on
				// another goroutine; dropping the map entry here would
				// orphan a live, still-launching process that awaitReady
				// will later return successfully but that no code path can
				// ever see or stop again (reaper/StopAll/budget accounting
				// all key off the map, not the process).
				if readyClosed && (!inst.healthy.Load() || inst.exited.Load()) {
					m.mu.Lock()
					m.dropLocked(alias, inst)
					m.mu.Unlock()
				}
			}
			if err := m.checkDeadline(ctx, alias, deadline); err != nil {
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
				case <-ctx.Done():
				case <-m.clock.After(m.timeUntil(deadline)):
				}
			}
			if err := m.checkDeadline(ctx, alias, deadline); err != nil {
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

		// awaitReady is deliberately not ctx-aware: this goroutine now owns
		// the launch it just started (launchLocked/cmd.Start already ran),
		// and the instance holds a lease with leases==0 nowhere in its
		// lifecycle until the health check resolves one way or the other.
		// Racing ctx against the poll would force releasing that lease on a
		// not-yet-healthy instance, making it eligible for LRU eviction
		// (lruIdleVictimLocked only checks leases/exited, not healthy) while
		// still mid-launch. A caller that disconnects here simply waits out
		// the (bounded, 120s) health check like any other in-process
		// operation with no cancellation seam.
		release := m.releaser(inst)
		if err := m.awaitReady(alias, inst); err != nil {
			release()
			return nil, nil, err
		}
		return endpointForPort(m.profile, inst.port), release, nil
	}
}

// checkDeadline converts an expired admission deadline or a cancelled ctx
// into a user-facing error naming the instances that were holding the
// budget.
func (m *ServerManager) checkDeadline(ctx context.Context, alias string, deadline time.Time) error {
	if ctx.Err() == nil && m.clock.Now().Before(deadline) {
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
	reason := "timed out"
	if ctx.Err() != nil {
		reason = "caller gave up"
	}
	if len(busy) > 0 {
		return fmt.Errorf("%s: %s waiting for capacity to run %q; busy: %s",
			m.profile.Kind, reason, alias, strings.Join(busy, ", "))
	}
	return fmt.Errorf("%s: %s waiting for capacity to run %q", m.profile.Kind, reason, alias)
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
		var err error
		port, err = m.allocatePort()
		if err != nil {
			return nil, fmt.Errorf("%s: cannot launch %q: %w", m.profile.Kind, alias, err)
		}
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
	// Explicit scrubbed env, not the exec.Command default of full
	// inheritance: llama-server/mlx-serve is a managed child like any
	// other spawn path and must not inherit relayLLM's own credentials
	// (internal bearer, project tokens) just because nothing here ever
	// set cmd.Env before.
	cmd.Env = spawn.ChildBaseEnv()
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

	// A successful launch retires any recorded failure for this alias,
	// whatever route triggered it.
	m.mu.Lock()
	delete(m.loadErrors, alias)
	m.mu.Unlock()

	slog.Info(fmt.Sprintf("%s: server ready", m.profile.Kind), "alias", alias, "port", inst.port)
	return nil
}

// fitsLocked reports whether launching alias would stay inside the budget.
// The alias's own existing instance (if any) is excluded from the totals since
// it is about to be replaced. Models with an unknown size (need == 0) are
// never blocked by the memory cap, only by the instance cap.
//
// This budget is built here rather than delegated to llama.cpp's own router
// mode (which ships on-demand launch, LRU eviction, and --models-max) for two
// reasons: it only knows GGUF, so mlx-serve would still need this manager,
// and it has no idle TTL — eviction fires only when a new model needs a
// slot, so "reclaim memory when nothing is running" would stay unimplemented.
// Revisit if llama.cpp's router grows an idle TTL and mlx-serve is dropped.
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

// ListModels returns types.ModelInfo entries for all configured models.
// Attachment support is per-model: present only when the user configured
// an mmproj (multimodal projector) for the underlying server.
func (m *ServerManager) ListModels() []types.ModelInfo {
	models := make([]types.ModelInfo, len(m.config.Models))
	for i, cfg := range m.config.Models {
		value := m.profile.Kind + "/" + cfg.Alias
		_, hasMmproj := cfg.Args["mmproj"]
		models[i] = types.ModelInfo{
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

// maxPortScanAttempts bounds allocatePort's search. Without a cap, a
// persistent non-EADDRINUSE Listen failure (fd exhaustion, or nextPort
// climbing past 65535) would spin forever holding m.mu, wedging every
// Acquire/ModelCatalog/ListInstances call on this manager.
const maxPortScanAttempts = 2000

// allocatePort finds the next free TCP port starting from m.nextPort.
// Must be called with m.mu held.
func (m *ServerManager) allocatePort() (int, error) {
	for i := 0; i < maxPortScanAttempts; i++ {
		port := m.nextPort
		m.nextPort++
		if port > 65535 {
			return 0, fmt.Errorf("%s: port scan exhausted the valid range (nextPort=%d)", m.profile.Kind, port)
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue // port occupied, try next
		}
		ln.Close()
		return port, nil
	}
	return 0, fmt.Errorf("%s: could not find a free port after %d attempts starting at %d", m.profile.Kind, maxPortScanAttempts, m.nextPort-maxPortScanAttempts)
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
func buildServerArgs(profile config.ServerProfile, args map[string]any, port int) []string {
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
			result = append(result, flag, fmt.Sprintf("%v", v))
		}
	}
	return result
}

// Model status values, matching llama.cpp router mode's /models vocabulary so
// clients written against it (pi's built-in llama.cpp extension, for one) can
// read our catalog unmodified.
const (
	ModelStatusLoaded   = "loaded"
	ModelStatusLoading  = "loading"
	ModelStatusUnloaded = "unloaded"
)

// ManagedModelInfo describes one configured alias for catalog listings.
// ContextSize is 0 when the model does not pin a ctx-size. json tags added
// for GET /api/status/detailed's models.catalog rows (status_metrics.go) —
// nothing else marshals this type today (relay_router_models.go and
// relay_router_anthropic.go both read fields by hand).
type ManagedModelInfo struct {
	Alias          string `json:"alias"`
	Status         string `json:"status"`
	Failed         bool   `json:"failed"`                   // last explicit load failed; clients stop polling on this
	Error          string `json:"error,omitempty"`          // failure detail, empty unless Failed
	ContextSize    int64  `json:"contextSize,omitempty"`    // configured ctx-size; 0 when unset
	TrainedContext int64  `json:"trainedContext,omitempty"` // the model's native context; 0 when unknown
	SupportsImages bool   `json:"supportsImages"`
}

// ModelCatalog returns every configured alias with its current load state.
// Unlike ListInstances, which only knows about processes that exist, this
// covers the whole configured set — a catalog listing needs the models you
// could load, not just the ones already running.
func (m *ServerManager) ModelCatalog() []ManagedModelInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]ManagedModelInfo, 0, len(m.config.Models))
	for _, cfg := range m.config.Models {
		// "loaded" here means usable right now, not resident right now.
		//
		// llama.cpp's router reports residency because a client there has to
		// ask for a load before it can use a model. We launch on demand: any
		// configured alias serves a request immediately, so from a client's
		// side there is nothing to wait for. Reporting residency instead would
		// also make models vanish from a client's picker whenever the idle
		// reaper reclaimed them — clients filter their model list to "loaded",
		// so a 30-minute lull would silently empty it.
		//
		// Actual residency is not hidden, just reported where it belongs:
		// /api/status instances (with leases, memory, and idle time).
		inst, running := m.instances[cfg.Alias]
		if running && inst.exited.Load() {
			running = false
		}

		status := ModelStatusLoaded
		if running && !inst.healthy.Load() {
			// A launch is genuinely in flight — clients poll this transition
			// after an explicit load, so it must be reported.
			status = ModelStatusLoading
		}

		entry := ManagedModelInfo{Alias: cfg.Alias, Status: status}
		// A recorded failure only speaks for the alias while nothing is
		// running for it; a live instance is proof the error is stale.
		if msg, failed := m.loadErrors[cfg.Alias]; failed && !running {
			// Demote from "usable" — it demonstrably is not — and flag it so a
			// client polling for readiness stops instead of spinning.
			entry.Status = ModelStatusUnloaded
			entry.Failed = true
			entry.Error = msg
		}
		if ctx, ok := numericArg(cfg.Args, "ctx-size"); ok && ctx > 0 {
			entry.ContextSize = int64(ctx)
		}
		entry.TrainedContext = m.trainedContext[cfg.Alias]
		_, entry.SupportsImages = cfg.Args["mmproj"]

		out = append(out, entry)
	}
	return out
}

// StartLoad begins loading alias in the background and returns immediately.
// Callers poll ModelCatalog for the outcome.
//
// Loading asynchronously is not an optimization — llama.cpp router mode's
// /models/load behaves this way, and clients written against it put a short
// timeout on the request itself (pi uses 15s). Loading a cold 40GB model
// synchronously would abort the caller's HTTP request long before the server
// finished starting.
func (m *ServerManager) StartLoad(alias string) error {
	if m.config.FindByAlias(alias) == nil {
		return fmt.Errorf("%s: unknown model alias %q", m.profile.Kind, alias)
	}

	m.mu.Lock()
	delete(m.loadErrors, alias)
	m.mu.Unlock()

	go func() {
		// No request is waiting on this — it's a fire-and-forget background
		// load — so there is nothing to bind the ctx to but the load's own
		// lifetime.
		_, release, err := m.Acquire(context.Background(), alias)
		if err != nil {
			slog.Warn(fmt.Sprintf("%s: explicit load failed", m.profile.Kind), "alias", alias, "error", err)
			m.mu.Lock()
			m.loadErrors[alias] = err.Error()
			m.mu.Unlock()
			return
		}
		// Drop the lease straight away. An explicit load pins nothing — it
		// makes the model resident, and the idle reaper or a budget eviction
		// reclaims it on the usual terms.
		release()
	}()
	return nil
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

func endpointForPort(profile config.ServerProfile, port int) *config.OpenAIEndpoint {
	return &config.OpenAIEndpoint{
		Name:    profile.Kind,
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d/v1", port),
		Group:   profile.Group,
	}
}

// maxLogLineBytes raises bufio.Scanner's default 64KB token limit. Verbose
// request logging (llama-server logs full request JSON, which easily
// exceeds 64KB with a long context or an image) would otherwise hit
// ErrTooLong, silently exit the scan loop, and stop draining the pipe —
// once the OS pipe buffer then fills, the child's next write blocks.
const maxLogLineBytes = 8 * 1024 * 1024

// logProcessOutput pipes cmd's stdout and stderr to slog, one line at a
// time via bufio.Scanner. This correctly handles partial writes and
// multi-line output, unlike a bare io.Writer.
func logProcessOutput(cmd *exec.Cmd, kind, alias string) {
	source := fmt.Sprintf("%s[%s]", kind, alias)
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		go func() {
			scanner := bufio.NewScanner(stdout)
			scanner.Buffer(make([]byte, 0, 64*1024), maxLogLineBytes)
			for scanner.Scan() {
				slog.Debug(scanner.Text(), "source", source)
			}
		}()
	}
	stderr, err := cmd.StderrPipe()
	if err == nil {
		go func() {
			scanner := bufio.NewScanner(stderr)
			scanner.Buffer(make([]byte, 0, 64*1024), maxLogLineBytes)
			for scanner.Scan() {
				slog.Warn(scanner.Text(), "source", source)
			}
		}()
	}
}
