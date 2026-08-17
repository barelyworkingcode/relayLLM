package main

import (
	"strings"
	"testing"
	"time"
)

// Hermetic tests for the managed-server memory budget: admission control,
// LRU eviction, lease accounting, and the idle reaper. None of these launch a
// process — instances are injected directly, which is what lets the eviction
// and lease logic be tested without a real llama-server.

// newBudgetManager builds a manager with the given caps and aliases. Every
// alias gets an explicit memoryGB so estimation never touches the filesystem.
func newBudgetManager(t *testing.T, cfg *ServerConfig, sizes map[string]float64) (*ServerManager, *FakeClock) {
	t.Helper()
	for alias, gb := range sizes {
		cfg.Models = append(cfg.Models, ServerModelConfig{
			Alias: alias,
			Args:  map[string]any{"memoryGB": gb, "model": "/nonexistent/" + alias + ".gguf"},
		})
	}
	// Pin a binary that cannot exist. Admission control runs before the launch,
	// so the interesting behavior is still exercised — and a developer with a
	// real llama-server on PATH does not get a 120s health poll against a
	// process spawned with a nonexistent model file.
	m := NewServerManager(llamaProfile, cfg, "/nonexistent/relayllm-test-server")
	clk := NewFakeClock(time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC))
	m.clock = clk
	return m, clk
}

// addInstance injects a healthy running instance for alias. cmd is nil, so
// StopInstance treats it as already-dead and simply unregisters it.
func addInstance(m *ServerManager, alias string, leases int, lastUsed time.Time) *serverInstance {
	inst := &serverInstance{
		config:    ServerModelConfig{Alias: alias},
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
	return inst
}

func loadedAliases(m *ServerManager) []string {
	var out []string
	for _, inst := range m.ListInstances() {
		out = append(out, inst.Alias)
	}
	return out
}

func TestAcquire_ReusesHealthyInstanceAndTracksLeases(t *testing.T) {
	m, clk := newBudgetManager(t, &ServerConfig{}, map[string]float64{"a": 10})
	inst := addInstance(m, "a", 0, clk.Now())

	endpoint, release, err := m.Acquire("a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !strings.Contains(endpoint.BaseURL, "9000") {
		t.Errorf("endpoint = %q, want the existing instance's port 9000", endpoint.BaseURL)
	}

	m.mu.Lock()
	got := inst.leases
	m.mu.Unlock()
	if got != 1 {
		t.Errorf("leases after Acquire = %d, want 1", got)
	}

	// Release must be idempotent: a double call cannot drive the count negative,
	// which would let a busy instance look idle to the reaper.
	release()
	release()

	m.mu.Lock()
	got = inst.leases
	m.mu.Unlock()
	if got != 0 {
		t.Errorf("leases after double release = %d, want 0", got)
	}
}

func TestAcquire_RejectsModelLargerThanEntireBudget(t *testing.T) {
	m, _ := newBudgetManager(t, &ServerConfig{MaxMemoryGB: 20}, map[string]float64{"huge": 41})

	_, _, err := m.Acquire("huge")
	if err == nil {
		t.Fatal("Acquire succeeded; want rejection for a model bigger than the budget")
	}
	// Failing fast matters: the alternative is blocking for the full admission
	// timeout waiting for an eviction that could never create enough room.
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("error = %q, want it to mention the budget", err)
	}
}

func TestAcquire_UnknownAlias(t *testing.T) {
	m, _ := newBudgetManager(t, &ServerConfig{}, map[string]float64{"a": 10})
	if _, _, err := m.Acquire("nope"); err == nil {
		t.Fatal("Acquire of an unconfigured alias should fail")
	}
}

func TestFitsLocked_CountAndMemoryCaps(t *testing.T) {
	tests := []struct {
		name      string
		cfg       ServerConfig
		loaded    map[string]float64 // alias → size already resident
		want      bool
		wantAlias string
		need      float64
	}{
		{
			name: "under both caps",
			cfg:  ServerConfig{MaxLoaded: 3, MaxMemoryGB: 100},
			// 26GB resident, adding 15GB stays under 100GB and 3 instances.
			loaded: map[string]float64{"big": 26}, need: 15, want: true,
		},
		{
			name:   "instance cap reached",
			cfg:    ServerConfig{MaxLoaded: 1},
			loaded: map[string]float64{"big": 26}, need: 1, want: false,
		},
		{
			name: "memory cap reached",
			cfg:  ServerConfig{MaxMemoryGB: 32},
			// 26 + 15 = 41GB, over the 32GB cap even though the count is fine.
			loaded: map[string]float64{"big": 26}, need: 15, want: false,
		},
		{
			name:   "no caps configured means unlimited",
			cfg:    ServerConfig{},
			loaded: map[string]float64{"a": 26, "b": 41, "c": 15}, need: 99, want: true,
		},
		{
			name: "unknown size is not blocked by the memory cap",
			cfg:  ServerConfig{MaxMemoryGB: 32},
			// need == 0 means "could not estimate"; the count cap still applies
			// but the byte cap cannot meaningfully judge it.
			loaded: map[string]float64{"big": 26}, need: 0, want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sizes := map[string]float64{"candidate": tc.need}
			for a, gb := range tc.loaded {
				sizes[a] = gb
			}
			cfg := tc.cfg
			m, clk := newBudgetManager(t, &cfg, sizes)
			for a := range tc.loaded {
				addInstance(m, a, 0, clk.Now())
			}

			m.mu.Lock()
			got := m.fitsLocked("candidate", int64(tc.need*bytesPerGB))
			m.mu.Unlock()

			if got != tc.want {
				t.Errorf("fitsLocked = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLRUVictim_PrefersOldestAndSkipsLeased(t *testing.T) {
	m, clk := newBudgetManager(t, &ServerConfig{}, map[string]float64{"old": 10, "new": 10, "busy": 10})

	// "busy" is both the oldest and leased — it must never be chosen, because
	// evicting it would kill an in-flight generation.
	addInstance(m, "busy", 1, clk.Now())
	clk.Advance(time.Minute)
	addInstance(m, "old", 0, clk.Now())
	clk.Advance(time.Minute)
	addInstance(m, "new", 0, clk.Now())

	m.mu.Lock()
	victim, ok := m.lruIdleVictimLocked("")
	m.mu.Unlock()

	if !ok {
		t.Fatal("expected an idle victim")
	}
	if victim != "old" {
		t.Errorf("victim = %q, want %q (oldest idle instance)", victim, "old")
	}
}

func TestLRUVictim_NoneWhenAllLeased(t *testing.T) {
	m, clk := newBudgetManager(t, &ServerConfig{}, map[string]float64{"a": 10, "b": 10})
	addInstance(m, "a", 1, clk.Now())
	addInstance(m, "b", 2, clk.Now())

	m.mu.Lock()
	_, ok := m.lruIdleVictimLocked("")
	m.mu.Unlock()

	if ok {
		t.Error("lruIdleVictimLocked returned a victim while every instance was leased")
	}
}

func TestReapIdle_StopsOnlyInstancesPastTheTimeout(t *testing.T) {
	m, clk := newBudgetManager(t, &ServerConfig{IdleTimeoutMinutes: 30},
		map[string]float64{"stale": 10, "busy": 10, "recent": 10})

	addInstance(m, "stale", 0, clk.Now())
	addInstance(m, "busy", 1, clk.Now()) // leased: never idle, regardless of age
	clk.Advance(31 * time.Minute)
	addInstance(m, "recent", 0, clk.Now())

	m.ReapIdle()

	got := loadedAliases(m)
	if len(got) != 2 {
		t.Fatalf("loaded after reap = %v, want 2 instances (busy, recent)", got)
	}
	for _, alias := range got {
		if alias == "stale" {
			t.Error("stale instance survived the idle reaper")
		}
	}
}

func TestReapIdle_NoOpWithoutTimeout(t *testing.T) {
	m, clk := newBudgetManager(t, &ServerConfig{}, map[string]float64{"a": 10})
	addInstance(m, "a", 0, clk.Now())
	clk.Advance(30 * 24 * time.Hour)

	m.ReapIdle()

	if len(loadedAliases(m)) != 1 {
		t.Error("instance was reaped even though no idle timeout is configured")
	}
}

func TestAcquire_EvictsIdleLRUToMakeRoom(t *testing.T) {
	m, clk := newBudgetManager(t, &ServerConfig{MaxLoaded: 1},
		map[string]float64{"resident": 10, "wanted": 10})
	addInstance(m, "resident", 0, clk.Now())

	// The launch itself fails (no llama-server binary on the test path), but
	// eviction happens before the launch, so the observable effect is that the
	// resident instance was stopped to make room.
	_, _, err := m.Acquire("wanted")
	if err == nil {
		t.Fatal("expected the launch to fail without a real binary")
	}

	if got := loadedAliases(m); len(got) != 0 {
		t.Errorf("loaded = %v, want the resident instance evicted to make room", got)
	}
}

func TestAcquire_TimesOutWhenEverythingIsBusy(t *testing.T) {
	m, clk := newBudgetManager(t, &ServerConfig{MaxLoaded: 1, AdmissionTimeoutSeconds: 120},
		map[string]float64{"busy": 10, "wanted": 10})
	addInstance(m, "busy", 1, clk.Now()) // mid-generation

	errCh := make(chan error, 1)
	go func() {
		_, _, err := m.Acquire("wanted")
		errCh <- err
	}()

	// Wait for the goroutine to park in its timeout select before advancing,
	// otherwise the clock moves before anyone is listening.
	waitForWaiters(t, clk, 1)
	clk.Advance(121 * time.Second)

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Acquire succeeded; want an admission timeout")
		}
		if !strings.Contains(err.Error(), "timed out") {
			t.Errorf("error = %q, want an admission timeout", err)
		}
		// The message should name what is holding the budget.
		if !strings.Contains(err.Error(), "busy") {
			t.Errorf("error = %q, want it to name the busy instance", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire did not return after the admission deadline passed")
	}

	// The busy instance must still be running — waiting, not killing, is the
	// whole point of the wait-for-idle policy.
	if got := loadedAliases(m); len(got) != 1 || got[0] != "busy" {
		t.Errorf("loaded = %v, want the busy instance untouched", got)
	}
}

func TestAcquire_ProceedsWhenBusyInstanceGoesIdle(t *testing.T) {
	m, clk := newBudgetManager(t, &ServerConfig{MaxLoaded: 1},
		map[string]float64{"busy": 10, "wanted": 10})
	inst := addInstance(m, "busy", 0, clk.Now())

	// Take a lease the way a live turn would.
	m.mu.Lock()
	m.leaseLocked(inst)
	m.mu.Unlock()
	release := m.releaser(inst)

	errCh := make(chan error, 1)
	go func() {
		_, _, err := m.Acquire("wanted")
		errCh <- err
	}()

	waitForWaiters(t, clk, 1)
	release() // turn finishes → broadcast → admission retries and evicts

	select {
	case err := <-errCh:
		// Still fails on the missing binary, but it got past admission, which
		// is what distinguishes this from the timeout case.
		if err == nil {
			t.Fatal("expected the launch to fail without a real binary")
		}
		if strings.Contains(err.Error(), "timed out") {
			t.Errorf("error = %q, want admission to have succeeded after release", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire did not wake after the lease was released")
	}
}

// waitForWaiters blocks until the fake clock has at least n outstanding
// waiters, so a test can advance time knowing the goroutine is parked.
func waitForWaiters(t *testing.T, clk *FakeClock, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if clk.Waiters() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d clock waiter(s); have %d", n, clk.Waiters())
}

func TestBuildServerArgs_OmitsBudgetOnlyKeys(t *testing.T) {
	args := buildServerArgs(llamaProfile, map[string]any{
		"memoryGB": 26.0,
		"ctx-size": 4096.0,
	}, 8090)

	for _, a := range args {
		if a == "--memoryGB" {
			t.Fatalf("memoryGB leaked into the server argv: %v", args)
		}
	}
	if !containsArg(args, "--ctx-size") {
		t.Errorf("args = %v, want real server flags preserved", args)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
