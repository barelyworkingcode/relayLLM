package servermanager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"relayllm/internal/config"
	"relayllm/internal/testutil"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const splashTestID = "acme/tiny-GGUF:Q4"

// newSplashManager wires a splash manager to a fake splash install.
func newSplashManager(t *testing.T, cfg *config.ServerConfig) (*ServerManager, *testutil.FakeSplash) {
	t.Helper()
	fake := testutil.NewFakeSplash(t)
	cfg.BinaryPath = fake.Binary
	cfg.BasePort = fake.BasePort
	m := NewServerManager(SplashProfile, cfg, "")
	m.SetSplashModelsRootForTest(fake.ModelsRoot)
	m.SetStopGraceForTest(200 * time.Millisecond)
	t.Cleanup(m.StopAll)
	return m, fake
}

func splashModel(alias string, args map[string]any) config.ServerModelConfig {
	return config.ServerModelConfig{Alias: alias, Args: args}
}

func launchSplash(t *testing.T, m *ServerManager, alias string) ServerInstanceInfo {
	t.Helper()
	_, release, err := m.Acquire(context.Background(), alias)
	if err != nil {
		t.Fatalf("Acquire(%q): %v", alias, err)
	}
	release()
	for _, inst := range m.ListInstances() {
		if inst.Alias == alias {
			return inst
		}
	}
	t.Fatalf("no instance for %q after Acquire", alias)
	return ServerInstanceInfo{}
}

func TestSplashLaunch_ArgvIsServeSubcommandWithAliasAsServedName(t *testing.T) {
	for _, userSet := range []bool{false, true} {
		t.Run(fmt.Sprintf("userServedName=%v", userSet), func(t *testing.T) {
			args := map[string]any{"model": splashTestID, "max-context": "140K"}
			if userSet {
				args["served-model-name"] = "something-else"
			}
			m, fake := newSplashManager(t, &config.ServerConfig{Models: []config.ServerModelConfig{splashModel("tiny", args)}})
			fake.Install(t, splashTestID, "model.json")

			inst := launchSplash(t, m, "tiny")
			argv := fake.Run(t, inst.Port).Args

			port := fmt.Sprint(inst.Port)
			if len(argv) < 3 || argv[0] != "serve" || argv[1] != "--port" || argv[2] != port {
				t.Fatalf("argv = %q, want it to start with serve --port %s", argv, port)
			}
			if n := len(argv); n < 2 || argv[n-2] != "--served-model-name" || argv[n-1] != "tiny" {
				t.Errorf("argv = %q, want it to end with --served-model-name tiny", argv)
			}
			count := 0
			for _, a := range argv {
				if a == "--served-model-name" {
					count++
				}
			}
			if count != 1 {
				t.Errorf("argv has %d --served-model-name flags, want 1: %q", count, argv)
			}
			if i := slices.Index(argv, "--model"); i < 0 || i+1 >= len(argv) || argv[i+1] != splashTestID {
				t.Errorf("argv = %q, want --model %s", argv, splashTestID)
			}
		})
	}
}

func processGone(pid int) bool {
	return syscall.Kill(pid, 0) == syscall.ESRCH
}

func TestSplashStop_LeavesNeitherWrapperNorNativeChild(t *testing.T) {
	m, fake := newSplashManager(t, &config.ServerConfig{Models: []config.ServerModelConfig{
		splashModel("tiny", map[string]any{"model": splashTestID}),
	}})
	fake.Install(t, splashTestID, "model.json")
	run := fake.Run(t, launchSplash(t, m, "tiny").Port)

	if err := m.StopInstance("tiny"); err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !(processGone(run.WrapperPID) && processGone(run.NativePID)) {
		time.Sleep(20 * time.Millisecond)
	}
	if !processGone(run.WrapperPID) {
		t.Errorf("wrapper pid %d still running after stop", run.WrapperPID)
	}
	if !processGone(run.NativePID) {
		t.Errorf("native child pid %d (ignores SIGTERM) still running after stop", run.NativePID)
	}
}

func TestSplashIdleTimeout_FromSettings(t *testing.T) {
	const model = `"models":[{"alias":"s","model":"acme/m","memoryGB":1}]`
	never := time.Duration(0)
	tests := []struct {
		name     string
		settings string
		llama    bool
		keptAt   time.Duration
		reapedAt time.Duration // 0: never reaped
	}{
		{"splash 1 minute", `{"splash-serve":{"idleTimeoutMinutes":1,` + model + `}}`, false, 59 * time.Second, time.Minute},
		{"splash explicit 0", `{"splash-serve":{"idleTimeoutMinutes":0,` + model + `}}`, false, 1000 * time.Hour, never},
		{"splash absent", `{"splash-serve":{` + model + `}}`, false, 59 * time.Minute, 60 * time.Minute},
		{"llama absent", `{"llama-server":{` + model + `}}`, true, 1000 * time.Hour, never},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(tc.settings), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadConfig(dir, "")
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			profile, section := SplashProfile, cfg.Splash
			if tc.llama {
				profile, section = LlamaProfile, cfg.Llama
			}
			m := NewServerManager(profile, section, "/nonexistent/relayllm-test-server")
			clk := testutil.NewFakeClock(time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC))
			m.SetClock(clk)
			m.InjectInstanceForTest("s", 0, clk.Now())

			clk.Advance(tc.keptAt)
			m.ReapIdle()
			if got := loadedAliases(m); len(got) != 1 {
				t.Fatalf("after %v idle: loaded = %v, want s still loaded", tc.keptAt, got)
			}
			if tc.reapedAt == never {
				return
			}
			clk.Advance(tc.reapedAt - tc.keptAt)
			m.ReapIdle()
			if got := loadedAliases(m); len(got) != 0 {
				t.Errorf("after %v idle: loaded = %v, want s reclaimed", tc.reapedAt, got)
			}
		})
	}
}

func TestSplashBudget_MemoryGBEvictsIdleSplashBesideLoadedLlama(t *testing.T) {
	m, fake := newSplashManager(t, &config.ServerConfig{MaxMemoryGB: 30, Models: []config.ServerModelConfig{
		splashModel("a", map[string]any{"model": "acme/a", "memoryGB": 20.0}),
		splashModel("b", map[string]any{"model": "acme/b", "memoryGB": 20.0}),
	}})
	fake.Install(t, "acme/a", "model.json")
	fake.Install(t, "acme/b", "model.json")
	llama, _ := newBudgetManager(t, &config.ServerConfig{MaxMemoryGB: 50}, map[string]float64{"big": 40})
	llama.InjectReadyInstanceForTest("big", 9001, 0)
	m.InjectInstanceForTest("a", 0, time.Now().Add(-time.Minute))

	launchSplash(t, m, "b")

	if got := loadedAliases(m); !slices.Equal(got, []string{"b"}) {
		t.Errorf("splash loaded = %v, want [b] (a evicted: 20+20 GB > 30 GB)", got)
	}
	if got := m.Budget().UsedMemoryGB; got != 20 {
		t.Errorf("splash used memory = %v GB, want 20 from memoryGB", got)
	}
	if got := loadedAliases(llama); !slices.Equal(got, []string{"big"}) {
		t.Errorf("llama loaded = %v, want [big] untouched", got)
	}
}

func TestSplashPreflight_ReportsInCatalogAndFailsWithoutSpawning(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"not downloaded", map[string]any{"model": splashTestID},
			`splash: model not downloaded: "acme/tiny-GGUF:Q4" (alias "s"); download it first with "splash serve --model acme/tiny-GGUF:Q4"`},
		{"no slash", map[string]any{"model": "tiny"},
			`splash: model "tiny" (alias "s") is not a Hugging Face repo id; use OWNER/REPO or OWNER/REPO:VARIANT`},
		{"extra segment", map[string]any{"model": "acme/tiny/extra"},
			`splash: model "acme/tiny/extra" (alias "s") is not a Hugging Face repo id; use OWNER/REPO or OWNER/REPO:VARIANT`},
		{"dot-dot owner", map[string]any{"model": "../tiny"},
			`splash: model "../tiny" (alias "s") is not a Hugging Face repo id; use OWNER/REPO or OWNER/REPO:VARIANT`},
		{"no model", map[string]any{"memoryGB": 1.0}, `splash: model entry "s" has no "model"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, fake := newSplashManager(t, &config.ServerConfig{Models: []config.ServerModelConfig{splashModel("s", tc.args)}})

			row := m.ModelCatalog()[0]
			if row.Status != ModelStatusUnloaded || !row.Failed || row.Error != tc.want {
				t.Errorf("catalog = %+v, want unloaded, failed, error %q", row, tc.want)
			}
			_, _, err := m.Acquire(context.Background(), "s")
			if err == nil || err.Error() != tc.want {
				t.Errorf("Acquire err = %v, want %q", err, tc.want)
			}
			if runs := fake.Runs(t); len(runs) != 0 {
				t.Errorf("splash was spawned %d time(s); preflight must fail before launch", len(runs))
			}
		})
	}
}

func TestSplashCatalog_InstalledOrUncheckableModelIsUsable(t *testing.T) {
	tests := []struct {
		name, id, marker string
		extra            map[string]any
	}{
		{"model.json layout", "acme/tiny-GGUF", "model.json", nil},
		{"manifest.json layout with variant", splashTestID, "manifest.json", nil},
		{"revision pinned, markers elsewhere", splashTestID, "", map[string]any{"revision": "abc123"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"model": tc.id}
			for k, v := range tc.extra {
				args[k] = v
			}
			m, fake := newSplashManager(t, &config.ServerConfig{Models: []config.ServerModelConfig{splashModel("s", args)}})
			if tc.marker != "" {
				fake.Install(t, tc.id, tc.marker)
			}
			if row := m.ModelCatalog()[0]; row.Status != ModelStatusLoaded || row.Failed || row.Error != "" {
				t.Errorf("catalog = %+v, want loaded with no error", row)
			}
		})
	}
}

func TestSplashCatalog_MissingBinaryReportedAfterLoad(t *testing.T) {
	fake := testutil.NewFakeSplash(t)
	fake.Install(t, splashTestID, "model.json")
	m := NewServerManager(SplashProfile, &config.ServerConfig{
		BinaryPath: "/nonexistent/splash-for-test",
		Models:     []config.ServerModelConfig{splashModel("s", map[string]any{"model": splashTestID})},
	}, "")
	m.SetSplashModelsRootForTest(fake.ModelsRoot)

	if err := m.StartLoad("s"); err != nil {
		t.Fatalf("StartLoad: %v", err)
	}
	testutil.WaitFor(t, 5*time.Second, func() bool { return m.ModelCatalog()[0].Failed })
	row := m.ModelCatalog()[0]
	if row.Status != ModelStatusUnloaded || !strings.Contains(row.Error, "/nonexistent/splash-for-test") || !strings.Contains(row.Error, "not found") {
		t.Errorf("catalog = %+v, want unloaded with a binary-not-found error", row)
	}
}

func TestSplashCatalog_ContextFromMaxContext(t *testing.T) {
	tests := []struct {
		value any
		want  int64
	}{
		{32768.0, 32768},
		{"auto", 0},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprint(tc.value), func(t *testing.T) {
			m, fake := newSplashManager(t, &config.ServerConfig{Models: []config.ServerModelConfig{
				splashModel("s", map[string]any{"model": splashTestID, "max-context": tc.value}),
			}})
			fake.Install(t, splashTestID, "model.json")
			if got := m.ModelCatalog()[0].ContextSize; got != tc.want {
				t.Errorf("context size = %d, want %d", got, tc.want)
			}
		})
	}
}
