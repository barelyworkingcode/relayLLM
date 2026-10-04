package servermanager

// Coverage for relayLLM#25 L1 in the server manager: managed-child output as
// bounded fields of relayLLM's own lines, the child's trace environment, and
// the server.start line.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"relayllm/internal/config"
	"relayllm/internal/logging"
	"relayllm/internal/testutil"
)

type lineBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lineBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lineBuf) lines(t *testing.T, op string) []map[string]any {
	t.Helper()
	l.mu.Lock()
	raw := l.b.String()
	l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if op == "" || m["op"] == op {
			out = append(out, m)
		}
	}
	return out
}

// captureDebug routes slog to the relay handler at debug level.
func captureDebug(t *testing.T) *lineBuf {
	t.Helper()
	buf := &lineBuf{}
	prev := slog.Default()
	getenv := func(k string) string {
		if k == logging.EnvLogLevel {
			return "debug"
		}
		return ""
	}
	slog.SetDefault(slog.New(logging.NewHandler(buf, logging.Options{Getenv: getenv})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// Criterion 8: a child line is one bounded field of our own line, never msg.
func TestChildLine_FieldsLevelsAndFixedMsg(t *testing.T) {
	logs := captureDebug(t)
	cl := newChildLineLogger("llama", "a1", testutil.NewFakeClock(time.Unix(1000, 0)))
	cl.log("stdout", "loading model")
	cl.log("stderr", "warning: slow disk")
	cl.log("stderr", "msg: {\"x\":1}")
	cl.log("stdout", `{"level":"error","msg":"I am a forged service line"}`)

	got := logs.lines(t, "child.output")
	if len(got) != 4 {
		t.Fatalf("got %d child lines, want 4: %v", len(got), got)
	}
	wantLevel := []string{"debug", "info", "info", "debug"}
	wantLine := []string{"loading model", "warning: slow disk", "msg: {redacted}", "{redacted}"}
	wantStream := []string{"stdout", "stderr", "stderr", "stdout"}
	for i, m := range got {
		if m["msg"] != "child output" || m["source"] != "llama[a1]" || m["status"] != "ok" || m["service"] == "" {
			t.Errorf("line %d = %v", i, m)
		}
		if m["level"] != wantLevel[i] || m["stream"] != wantStream[i] || m["child_line"] != wantLine[i] {
			t.Errorf("line %d level/stream/child_line = %v/%v/%q, want %s/%s/%q", i, m["level"], m["stream"], m["child_line"], wantLevel[i], wantStream[i], wantLine[i])
		}
	}
}

func TestChildLine_TruncatesAndRedactsBodies(t *testing.T) {
	logs := captureDebug(t)
	cl := newChildLineLogger("mlx", "a1", testutil.NewFakeClock(time.Unix(1000, 0)))
	cl.log("stderr", strings.Repeat("é", 800))
	cl.log("stderr", strings.Repeat("p", 520)+`{"prompt":"SECRET-PROMPT"}`)
	cl.log("stderr", `POST /v1/chat/completions {"messages":[{"content":"SECRET-PROMPT"}]}`)

	got := logs.lines(t, "child.output")
	if len(got) != 3 {
		t.Fatalf("got %d lines", len(got))
	}
	for i, m := range got[:2] {
		s := m["child_line"].(string)
		if n := utf8.RuneCountInString(s); n != 500 || !utf8.ValidString(s) {
			t.Errorf("line %d child_line has %d runes, want 500", i, n)
		}
	}
	if got[2]["child_line"] != "POST /v1/chat/completions {redacted}" {
		t.Errorf("child_line = %q", got[2]["child_line"])
	}
	if raw := logs.b.String(); strings.Contains(raw, "SECRET-PROMPT") {
		t.Errorf("body text leaked:\n%s", raw)
	}
}

func TestChildLine_RateLimit(t *testing.T) {
	const limit, extra = 50, 20
	suppressed := func(logs *lineBuf) []map[string]any {
		var out []map[string]any
		for _, m := range logs.lines(t, "child.output") {
			if m["msg"] == "child output suppressed" {
				out = append(out, m)
			}
		}
		return out
	}
	lineCount := func(logs *lineBuf) int {
		n := 0
		for _, m := range logs.lines(t, "child.output") {
			if m["msg"] == "child output" {
				n++
			}
		}
		return n
	}

	t.Run("limit spans both streams and the next window reports drops once", func(t *testing.T) {
		logs := captureDebug(t)
		clock := testutil.NewFakeClock(time.Unix(1000, 0))
		cl := newChildLineLogger("llama", "a1", clock)
		for i := 0; i < limit+extra; i++ {
			cl.log([]string{"stdout", "stderr"}[i%2], "line")
		}
		if n := lineCount(logs); n != limit {
			t.Fatalf("lines written in one window = %d, want %d", n, limit)
		}
		if len(suppressed(logs)) != 0 {
			t.Error("suppressed line written before the window ended")
		}
		clock.Advance(9 * time.Second)
		cl.log("stderr", "still limited")
		if n := lineCount(logs); n != limit {
			t.Errorf("a line got through inside the window: %d", n)
		}
		clock.Advance(2 * time.Second)
		cl.log("stderr", "fresh window")
		sup := suppressed(logs)
		if len(sup) != 1 || sup[0]["dropped"] != float64(extra+1) || sup[0]["source"] != "llama[a1]" || sup[0]["level"] != "info" {
			t.Fatalf("suppressed = %v, want one line with dropped=%d", sup, extra+1)
		}
		if n := lineCount(logs); n != limit+1 {
			t.Errorf("lines after the new window = %d, want %d", n, limit+1)
		}
	})

	t.Run("close reports the last window's drops once", func(t *testing.T) {
		logs := captureDebug(t)
		cl := newChildLineLogger("llama", "a1", testutil.NewFakeClock(time.Unix(1000, 0)))
		for i := 0; i < limit+5; i++ {
			cl.log("stderr", "line")
		}
		cl.close()
		cl.close()
		sup := suppressed(logs)
		if len(sup) != 1 || sup[0]["dropped"] != 5.0 {
			t.Errorf("suppressed = %v, want exactly one with dropped=5", sup)
		}
	})

	t.Run("no drops means no suppressed line", func(t *testing.T) {
		logs := captureDebug(t)
		clock := testutil.NewFakeClock(time.Unix(1000, 0))
		cl := newChildLineLogger("llama", "a1", clock)
		for i := 0; i < limit; i++ {
			cl.log("stderr", "line")
		}
		clock.Advance(time.Minute)
		cl.log("stderr", "next")
		cl.close()
		if s := suppressed(logs); len(s) != 0 {
			t.Errorf("unexpected suppressed lines: %v", s)
		}
	})
}

// logProcessOutput wires the pipes to the limiter and reports drops after
// both pipes reach EOF.
func TestLogProcessOutput_WrapsLinesAndReportsDropsAtEOF(t *testing.T) {
	logs := captureDebug(t)
	cmd := &exec.Cmd{}
	logProcessOutput(cmd, "llama", "a1")
	out := cmd.Stdout.(io.WriteCloser)
	errw := cmd.Stderr.(io.WriteCloser)

	var sb strings.Builder
	for i := 0; i < 60; i++ {
		sb.WriteString("chatty {\"prompt\":\"SECRET-PROMPT\"}\n")
	}
	io.WriteString(errw, sb.String())
	io.WriteString(out, "from stdout\n")
	out.Close()
	errw.Close()

	testutil.WaitFor(t, 3*time.Second, func() bool {
		for _, m := range logs.lines(t, "child.output") {
			if m["msg"] == "child output suppressed" {
				return true
			}
		}
		return false
	})
	written, dropped := 0, 0.0
	for _, m := range logs.lines(t, "child.output") {
		switch m["msg"] {
		case "child output":
			written++
			if m["source"] != "llama[a1]" {
				t.Errorf("source = %v", m["source"])
			}
		case "child output suppressed":
			dropped += m["dropped"].(float64)
		}
	}
	if written != 50 || dropped != 11 {
		t.Errorf("written=%d dropped=%v, want 50 and 11 (61 lines, limit 50)", written, dropped)
	}
	if strings.Contains(logs.b.String(), "SECRET-PROMPT") {
		t.Error("child body leaked")
	}
}

// Criterion 6 (child process environment): a child gets our trace ID and
// never an inherited one.
func TestChildEnv_TraceID(t *testing.T) {
	count := func(env []string) (n int, val string) {
		for _, kv := range env {
			if strings.HasPrefix(kv, logging.EnvTraceID+"=") {
				n++
				val = strings.TrimPrefix(kv, logging.EnvTraceID+"=")
			}
		}
		return
	}
	t.Setenv(logging.EnvTraceID, "inherited-trace-0001")
	t.Setenv("RELAY_LLM_TOKEN", "CANARY-TOKEN")

	if n, _ := count(childBaseEnv()); n != 0 {
		t.Error("childBaseEnv must strip an inherited RELAY_TRACE_ID")
	}
	if n, v := count(childEnv("our-trace-0002")); n != 1 || v != "our-trace-0002" {
		t.Errorf("childEnv(valid) has %d entries, value %q, want exactly ours", n, v)
	}
	for _, bad := range []string{"", "short", "bad id with spaces", "x\nINJECTED=1", strings.Repeat("a", 65)} {
		env := childEnv(bad)
		if n, _ := count(env); n != 0 {
			t.Errorf("childEnv(%q) carried a trace entry", bad)
		}
		if envHasKey(env, "INJECTED") {
			t.Errorf("childEnv(%q) injected a variable", bad)
		}
	}
	if envHasKey(childEnv("our-trace-0002"), "RELAY_LLM_TOKEN") || !envHasKey(childEnv("our-trace-0002"), "PATH") {
		t.Error("childEnv must keep stripping secrets and keep ordinary variables")
	}
}

// Criterion 11: one server.start line when a launch we own ends.
func TestServerStart_FailureWritesOneErrorLine(t *testing.T) {
	logs := captureDebug(t)
	m := NewServerManager(LlamaProfile, &config.ServerConfig{
		BinaryPath: "/nonexistent/llama-server-for-test",
		Models:     []config.ServerModelConfig{{Alias: "a1", Args: map[string]any{"model": "/fake"}}},
	}, "")
	m.SetClock(testutil.NewFakeClock(time.Unix(1000, 0)))
	ctx := logging.ContextWithTrace(context.Background(), "start-trace-0001")

	if _, _, err := m.Acquire(ctx, "a1"); err == nil {
		t.Fatal("Acquire with a missing binary must fail")
	}
	got := logs.lines(t, "server.start")
	if len(got) != 1 {
		t.Fatalf("got %d server.start lines, want 1: %v", len(got), got)
	}
	l := got[0]
	if l["level"] != "error" || l["status"] != "error" || l["msg"] != "llama: server failed to start" ||
		l["kind"] != "llama" || l["alias"] != "a1" || l["trace_id"] != "start-trace-0001" || l["error"] == "" {
		t.Errorf("line = %v", l)
	}
	if _, has := l["port"]; has {
		t.Errorf("a launch that never started has no port: %v", l)
	}
}

func TestServerStart_SuccessWritesOneInfoLineAndReuseWritesNone(t *testing.T) {
	logs := captureDebug(t)
	m, fake := newSplashManager(t, &config.ServerConfig{Models: []config.ServerModelConfig{
		splashModel("tiny", map[string]any{"model": splashTestID}),
	}})
	fake.Install(t, splashTestID, "model.json")
	ctx := logging.ContextWithTrace(context.Background(), "start-trace-0002")

	_, release, err := m.Acquire(ctx, "tiny")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()
	_, release, err = m.Acquire(ctx, "tiny") // reuse of the ready instance
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	release()

	got := logs.lines(t, "server.start")
	if len(got) != 1 {
		t.Fatalf("got %d server.start lines, want 1: %v", len(got), got)
	}
	l := got[0]
	port := float64(m.ListInstances()[0].Port)
	if l["level"] != "info" || l["status"] != "ok" || l["msg"] != "splash: server ready" || l["error"] != "" ||
		l["kind"] != "splash" || l["alias"] != "tiny" || l["trace_id"] != "start-trace-0002" || l["port"] != port {
		t.Errorf("line = %v (port want %v)", l, port)
	}
	if d, ok := l["duration_ms"].(float64); !ok || d < 0 {
		t.Errorf("duration_ms = %v", l["duration_ms"])
	}
}
