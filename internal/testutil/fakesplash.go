package testutil

// FakeSplash stands in for the splash launcher without running it: a wrapper
// that serves the OpenAI API and spawns a "native" child in its own process
// group, as the real launcher does. The native child ignores SIGTERM, so only
// a group-wide SIGKILL removes it. Both roles are the test binary itself,
// re-executed; a package using this must call RunFakeSplashIfRequested first
// thing in its TestMain.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	fakeSplashRoleEnv  = "RELAYLLM_FAKESPLASH_ROLE"
	fakeSplashStateEnv = "RELAYLLM_FAKESPLASH_STATE"
	// fakeSplashLifetime bounds a leaked fake if a test dies mid-run.
	fakeSplashLifetime = 5 * time.Minute
)

// RunFakeSplashIfRequested runs the fake wrapper or native role and exits when
// this process was launched as one; otherwise it returns.
func RunFakeSplashIfRequested() {
	switch os.Getenv(fakeSplashRoleEnv) {
	case "wrapper":
		runFakeSplashWrapper(os.Args[1:])
		os.Exit(0)
	case "native":
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(fakeSplashLifetime)
		os.Exit(0)
	}
}

// FakeSplash is one fake install: a wrapper script, a scratch base port and
// an empty model store.
type FakeSplash struct {
	Binary     string
	BasePort   int
	ModelsRoot string
	state      string
}

// FakeSplashRun is what one launch recorded: its argv (after the binary) and
// the wrapper's and native child's PIDs.
type FakeSplashRun struct {
	Args       []string `json:"args"`
	WrapperPID int      `json:"wrapperPid"`
	NativePID  int      `json:"nativePid"`
}

func NewFakeSplash(t *testing.T) *FakeSplash {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("fake splash: %v", err)
	}
	dir := t.TempDir()
	f := &FakeSplash{
		Binary:     filepath.Join(dir, "splash"),
		ModelsRoot: filepath.Join(dir, "models"),
		state:      filepath.Join(dir, "state"),
	}
	for _, d := range []string{f.ModelsRoot, f.state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := fmt.Sprintf("#!/bin/sh\n%s=wrapper %s=%s exec %s \"$@\"\n",
		fakeSplashRoleEnv, fakeSplashStateEnv, shQuote(f.state), shQuote(exe))
	if err := os.WriteFile(f.Binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.BasePort = ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	t.Cleanup(func() {
		for _, r := range f.Runs(t) {
			_ = syscall.Kill(r.WrapperPID, syscall.SIGKILL)
			_ = syscall.Kill(r.NativePID, syscall.SIGKILL)
		}
	})
	return f
}

// Install marks id as downloaded using the given marker file name.
func (f *FakeSplash) Install(t *testing.T, id, marker string) {
	t.Helper()
	dir := filepath.Join(f.ModelsRoot, filepath.FromSlash(id))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, marker), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Run returns the launch that served port.
func (f *FakeSplash) Run(t *testing.T, port int) FakeSplashRun {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.state, fmt.Sprintf("%d.json", port)))
	if err != nil {
		t.Fatalf("fake splash: no launch recorded for port %d: %v", port, err)
	}
	var r FakeSplashRun
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// Runs returns every launch recorded so far.
func (f *FakeSplash) Runs(t *testing.T) []FakeSplashRun {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(f.state, "*.json"))
	out := make([]FakeSplashRun, 0, len(paths))
	for _, p := range paths {
		var r FakeSplashRun
		if data, err := os.ReadFile(p); err == nil && json.Unmarshal(data, &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func runFakeSplashWrapper(args []string) {
	time.AfterFunc(fakeSplashLifetime, func() { os.Exit(0) })
	port, host, served := flagValue(args, "--port"), flagValue(args, "--host"), flagValue(args, "--served-model-name")

	exe, _ := os.Executable()
	native := exec.Command(exe)
	native.Env = append(os.Environ(), fakeSplashRoleEnv+"=native")
	if err := native.Start(); err != nil {
		os.Exit(3)
	}
	rec, _ := json.Marshal(FakeSplashRun{Args: args, WrapperPID: os.Getpid(), NativePID: native.Process.Pid})
	state := os.Getenv(fakeSplashStateEnv)
	tmp := filepath.Join(state, port+".tmp")
	if os.WriteFile(tmp, rec, 0o644) != nil || os.Rename(tmp, filepath.Join(state, port+".json")) != nil {
		os.Exit(4)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Like Splash: answer only to the served-model-name.
		if req.Model != served {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"model_not_found"}}`))
			return
		}
		last := ""
		if n := len(req.Messages); n > 0 {
			last = req.Messages[n-1].Content
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, piece := range []string{"echo: ", last} {
			chunk, _ := json.Marshal(map[string]any{
				"id": "fake-1", "object": "chat.completion.chunk", "model": served,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": piece}}},
			})
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		os.Exit(5)
	}
	_ = http.Serve(ln, mux)
}

// flagValue returns the value after the last occurrence of flag.
func flagValue(args []string, flag string) string {
	v := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			v = args[i+1]
		}
	}
	return v
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
