package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := strings.TrimSuffix(s.b.String(), "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func newLogger(m map[string]string, now func() time.Time) (*slog.Logger, *Handler, *syncBuf) {
	buf := &syncBuf{}
	h := NewHandler(buf, Options{DefaultService: DefaultService, Getenv: env(m), Now: now})
	return slog.New(h), h, buf
}

func parse(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("not JSON: %q: %v", line, err)
	}
	return m
}

func one(t *testing.T, buf *syncBuf) map[string]any {
	t.Helper()
	ls := buf.lines()
	if len(ls) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(ls), ls)
	}
	return parse(t, ls[0])
}

var nineKeys = []string{"ts", "level", "msg", "service", "op", "status", "duration_ms", "error", "trace_id"}

// Criteria 1, 2, 13: every line shape the service writes validates against
// the relay schema and starts with the nine keys in order.
func TestSchema_SampleLinesFromEveryShape(t *testing.T) {
	schema := loadSchema(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ctx := ContextWithTrace(context.Background(), "0123456789abcdef0123456789abcdef")
	lg, _, buf := newLogger(map[string]string{EnvLogLevel: "debug"}, func() time.Time { return now })

	lg.Debug("a debug line")
	lg.Info("an info line")
	lg.Warn("a warn line")
	lg.Error("an error line", "error", errors.New("boom"))
	lg.InfoContext(ctx, "model request", "op", "model.request", "status", "ok", "duration_ms", int64(12), "error", "",
		"method", "POST", "path", "/v1/chat/completions", "http_status", 200, "transport", "tcp", "target", "llama")
	lg.LogAttrs(ctx, slog.LevelWarn, "model request", slog.String("op", "model.request"), slog.String("status", "denied"),
		slog.String("error", "http_401"), slog.Int("status_code", 401))
	lg.InfoContext(ctx, "llama: server ready", "op", "server.start", "status", "ok", "duration_ms", 1500*time.Millisecond, "kind", "llama", "alias", "a1", "port", 9001)
	lg.ErrorContext(ctx, "llama: server failed to start", "op", "server.start", "status", "error", "error", "no such file", "kind", "llama", "alias", "a1")
	lg.Info("child output", "op", "child.output", "status", "ok", "source", "llama[a1]", "stream", "stderr", "child_line", "listening on {redacted}")
	lg.Info("child output suppressed", "op", "child.output", "status", "ok", "source", "llama[a1]", "dropped", 7)
	// Hostile values must still produce a valid line.
	lg.Info("hostile", "ts", "x", "op", "Not An Op", "status", 500, "duration_ms", -5, "service", "")

	// invalid level and the debug-window expiry lines
	buf2 := &syncBuf{}
	clock2 := now
	h2 := NewHandler(buf2, Options{Getenv: env(map[string]string{EnvLogLevel: "debug"}), Now: func() time.Time { return clock2 }})
	clock2 = now.Add(DebugWindow)
	slog.New(h2).Info("after window")
	buf3 := &syncBuf{}
	withDefaultLogger(t, func() { Install(buf3, Options{Getenv: env(map[string]string{EnvLogLevel: "loud"})}) })

	all := append(append(buf.lines(), buf2.lines()...), buf3.lines()...)
	if len(all) < 14 {
		t.Fatalf("expected at least 14 sample lines, got %d", len(all))
	}
	for _, l := range all {
		checkLine(t, schema, l)
		if got := keyOrder(t, l)[:9]; !slices.Equal(got, nineKeys) {
			t.Errorf("key order = %v, want %v first: %s", got, nineKeys, l)
		}
	}
}

func withDefaultLogger(t *testing.T, f func()) {
	t.Helper()
	prev := slog.Default()
	defer slog.SetDefault(prev)
	f()
}

func TestWriter_Rules(t *testing.T) {
	long := strings.Repeat("é", 600)
	ctx := ContextWithTrace(context.Background(), "trace-abc-12345")
	cases := []struct {
		name string
		env  map[string]string
		ctx  context.Context
		lvl  slog.Level
		msg  string
		args []any
		want map[string]any
	}{
		{name: "defaults at info", lvl: slog.LevelInfo, msg: "hi",
			want: map[string]any{"level": "info", "msg": "hi", "service": "relayllm", "op": "log", "status": "ok", "duration_ms": 0.0, "error": "", "trace_id": ""}},
		{name: "default status at warn is error", lvl: slog.LevelWarn, msg: "w", want: map[string]any{"level": "warn", "status": "error"}},
		{name: "default status at error is error", lvl: slog.LevelError, msg: "e", want: map[string]any{"level": "error", "status": "error"}},
		{name: "trace from context", ctx: ctx, lvl: slog.LevelInfo, msg: "t", want: map[string]any{"trace_id": "trace-abc-12345"}},
		{name: "service from env", env: map[string]string{EnvServiceID: "acme-llm"}, lvl: slog.LevelInfo, msg: "s", want: map[string]any{"service": "acme-llm"}},
		{name: "reserved names renamed", lvl: slog.LevelInfo, msg: "r",
			args: []any{"ts", "T", "level", "L", "msg", "M", "service", "S", "trace_id", "I"},
			want: map[string]any{"attr_ts": "T", "attr_level": "L", "attr_msg": "M", "attr_service": "S", "attr_trace_id": "I", "msg": "r", "service": "relayllm", "level": "info", "trace_id": ""}},
		{name: "valid op kept", lvl: slog.LevelInfo, msg: "o", args: []any{"op", "model.request"}, want: map[string]any{"op": "model.request"}},
		{name: "invalid op moved", lvl: slog.LevelInfo, msg: "o", args: []any{"op", "Bad Op!"}, want: map[string]any{"op": "log", "attr_op": "Bad Op!"}},
		{name: "numeric status becomes http_status", lvl: slog.LevelInfo, msg: "s", args: []any{"status", 404}, want: map[string]any{"status": "ok", "http_status": 404.0}},
		{name: "odd string status moved", lvl: slog.LevelInfo, msg: "s", args: []any{"status", "weird"}, want: map[string]any{"status": "ok", "attr_status": "weird"}},
		{name: "denied status kept", lvl: slog.LevelWarn, msg: "s", args: []any{"status", "denied"}, want: map[string]any{"status": "denied"}},
		{name: "duration int", lvl: slog.LevelInfo, msg: "d", args: []any{"duration_ms", 42}, want: map[string]any{"duration_ms": 42.0}},
		{name: "duration uint", lvl: slog.LevelInfo, msg: "d", args: []any{"duration_ms", uint(7)}, want: map[string]any{"duration_ms": 7.0}},
		{name: "duration time.Duration to ms", lvl: slog.LevelInfo, msg: "d", args: []any{"duration_ms", 1500 * time.Millisecond}, want: map[string]any{"duration_ms": 1500.0}},
		{name: "negative duration moved", lvl: slog.LevelInfo, msg: "d", args: []any{"duration_ms", -3}, want: map[string]any{"duration_ms": 0.0, "attr_duration_ms": -3.0}},
		{name: "non-numeric duration moved", lvl: slog.LevelInfo, msg: "d", args: []any{"duration_ms", "fast"}, want: map[string]any{"duration_ms": 0.0, "attr_duration_ms": "fast"}},
		{name: "error value stringified", lvl: slog.LevelWarn, msg: "e", args: []any{"error", errors.New("boom")}, want: map[string]any{"error": "boom"}},
		{name: "caller attrs kept", lvl: slog.LevelInfo, msg: "c", args: []any{"alias", "a1", "port", 9001}, want: map[string]any{"alias": "a1", "port": 9001.0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lg, _, buf := newLogger(c.env, nil)
			cx := c.ctx
			if cx == nil {
				cx = context.Background()
			}
			lg.Log(cx, c.lvl, c.msg, c.args...)
			got := one(t, buf)
			for k, w := range c.want {
				if got[k] != w {
					t.Errorf("%s = %#v, want %#v (line %v)", k, got[k], w, got)
				}
			}
		})
	}

	t.Run("msg and error truncated to 500 runes", func(t *testing.T) {
		lg, _, buf := newLogger(nil, nil)
		lg.Warn(long, "error", errors.New(long))
		got := one(t, buf)
		for _, k := range []string{"msg", "error"} {
			s := got[k].(string)
			if utf8.RuneCountInString(s) != 500 || !utf8.ValidString(s) {
				t.Errorf("%s has %d runes, want 500 valid", k, utf8.RuneCountInString(s))
			}
		}
	})

	t.Run("one newline-terminated line even with newlines in text", func(t *testing.T) {
		lg, _, buf := newLogger(nil, nil)
		lg.Info("a\nb", "note", "x\ny")
		if !strings.HasSuffix(buf.b.String(), "}\n") || len(buf.lines()) != 1 {
			t.Errorf("output = %q, want exactly one line", buf.b.String())
		}
	})

	t.Run("ts is UTC with milliseconds", func(t *testing.T) {
		_, h, buf := newLogger(nil, nil)
		loc := time.FixedZone("x", 2*3600)
		rec := slog.NewRecord(time.Date(2026, 1, 2, 5, 4, 5, 678_900_000, loc), slog.LevelInfo, "t", 0)
		if err := h.Handle(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
		if got := one(t, buf)["ts"]; got != "2026-01-02T03:04:05.678Z" {
			t.Errorf("ts = %v", got)
		}
	})

	t.Run("service option default used when env unset", func(t *testing.T) {
		buf := &syncBuf{}
		slog.New(NewHandler(buf, Options{DefaultService: "acme-default", Getenv: env(nil)})).Info("x")
		if got := one(t, buf)["service"]; got != "acme-default" {
			t.Errorf("service = %v", got)
		}
	})
}

func TestLevel_EnvHandling(t *testing.T) {
	cases := []struct {
		env          string
		debug, info  bool
		warn, errLvl bool
	}{
		{"", false, true, true, true},
		{"info", false, true, true, true},
		{" DEBUG ", true, true, true, true},
		{"warn", false, false, true, true},
		{"Error", false, false, false, true},
		{"loud", false, true, true, true}, // invalid: info
	}
	for _, c := range cases {
		t.Run("RELAY_LOG_LEVEL="+c.env, func(t *testing.T) {
			lg, _, buf := newLogger(map[string]string{EnvLogLevel: c.env}, nil)
			lg.Debug("d")
			lg.Info("i")
			lg.Warn("w")
			lg.Error("e")
			var got []string
			for _, l := range buf.lines() {
				got = append(got, parse(t, l)["msg"].(string))
			}
			want := map[string]bool{"d": c.debug, "i": c.info, "w": c.warn, "e": c.errLvl}
			for m, on := range want {
				if on != slices.Contains(got, m) {
					t.Errorf("message %q emitted=%v, want %v", m, !on, on)
				}
			}
		})
	}
}

func TestInstall_InvalidLevelWarnsOnceWithoutEchoingValue(t *testing.T) {
	withDefaultLogger(t, func() {
		buf := &syncBuf{}
		Install(buf, Options{Getenv: env(map[string]string{EnvLogLevel: "S3CRET-level"})})
		slog.Debug("not shown")
		got := one(t, buf)
		if got["level"] != "warn" || got["op"] != "log.level" || got["status"] != "error" || got["error"] != "invalid_level" {
			t.Errorf("line = %v", got)
		}
		if strings.Contains(strings.Join(buf.lines(), ""), "S3CRET") {
			t.Error("invalid level value was echoed")
		}
	})
}

func TestInstall_ValidLevelWritesNothingAndSetsDefault(t *testing.T) {
	withDefaultLogger(t, func() {
		buf := &syncBuf{}
		Install(buf, Options{Getenv: env(nil)})
		slog.Info("via default")
		if got := one(t, buf); got["msg"] != "via default" || got["op"] != "log" {
			t.Errorf("line = %v", got)
		}
	})
}

// Criterion 4: debug returns to info after 30 minutes and logs one warn line.
func TestDebugWindow(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := t0
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	set := func(d time.Duration) { mu.Lock(); now = t0.Add(d); mu.Unlock() }
	expiryLines := func(buf *syncBuf) int {
		n := 0
		for _, l := range buf.lines() {
			if m := parse(t, l); m["op"] == "log.level" && m["error"] == "debug_window_expired" && m["level"] == "warn" {
				n++
			}
		}
		return n
	}

	t.Run("no expiry at 29m59s", func(t *testing.T) {
		now = t0
		lg, _, buf := newLogger(map[string]string{EnvLogLevel: "debug"}, clock)
		set(DebugWindow - time.Second)
		lg.Debug("still debug")
		if n := expiryLines(buf); n != 0 {
			t.Errorf("expiry lines = %d, want 0", n)
		}
		if len(buf.lines()) != 1 {
			t.Errorf("debug line not written before the window ends: %q", buf.lines())
		}
	})

	t.Run("exactly one warn at 30m then info", func(t *testing.T) {
		now = t0
		lg, _, buf := newLogger(map[string]string{EnvLogLevel: "debug"}, clock)
		set(DebugWindow)
		lg.Debug("after window")
		lg.Info("info ok")
		lg.Debug("again")
		if n := expiryLines(buf); n != 1 {
			t.Errorf("expiry lines = %d, want 1: %q", n, buf.lines())
		}
		for _, l := range buf.lines() {
			if m := parse(t, l); m["msg"] == "after window" || m["msg"] == "again" {
				t.Errorf("debug line written after window: %s", l)
			}
		}
	})

	t.Run("concurrent callers at the deadline write one warn", func(t *testing.T) {
		now = t0
		lg, _, buf := newLogger(map[string]string{EnvLogLevel: "debug"}, clock)
		set(DebugWindow + time.Minute)
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); lg.Debug("d"); lg.Info("i") }()
		}
		wg.Wait()
		if n := expiryLines(buf); n != 1 {
			t.Errorf("expiry lines = %d, want 1", n)
		}
	})

	t.Run("info level never writes an expiry line", func(t *testing.T) {
		now = t0
		lg, _, buf := newLogger(nil, clock)
		set(3 * time.Hour)
		lg.Info("x")
		if n := expiryLines(buf); n != 0 {
			t.Errorf("expiry lines = %d, want 0", n)
		}
	})
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"error": slog.LevelError, " WARN": slog.LevelWarn, "Info": slog.LevelInfo, "debug ": slog.LevelDebug} {
		if got, ok := ParseLevel(in); !ok || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "trace", "warning", "3"} {
		if _, ok := ParseLevel(in); ok {
			t.Errorf("ParseLevel(%q) accepted", in)
		}
	}
}

func TestTruncate(t *testing.T) {
	for _, c := range []struct {
		in   string
		runs int
	}{{"", 0}, {strings.Repeat("a", 499), 499}, {strings.Repeat("a", 500), 500}, {strings.Repeat("a", 501), 500}, {strings.Repeat("日", 800), 500}} {
		got := Truncate(c.in)
		if n := utf8.RuneCountInString(got); n != c.runs || !utf8.ValidString(got) {
			t.Errorf("Truncate(%d bytes) = %d runes, want %d", len(c.in), n, c.runs)
		}
	}
}

func TestTraceIDs(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{32}$`)
	a, b := NewTraceID(), NewTraceID()
	if !re.MatchString(a) || !re.MatchString(b) || a == b {
		t.Errorf("NewTraceID gave %q, %q", a, b)
	}
	valid := []string{"abcd1234", "A_b-c_d-1", strings.Repeat("a", 64), a}
	invalid := []string{"", "short", strings.Repeat("a", 65), "has space 123", "new\nline123", "uni-é-code-123", "semi;colon1", `q"uote123`}
	for _, id := range valid {
		if !ValidTraceID(id) || TraceIDOrNew(id) != id {
			t.Errorf("%q should be accepted unchanged", id)
		}
	}
	for _, id := range invalid {
		if ValidTraceID(id) {
			t.Errorf("%q should be rejected", id)
		}
		if got := TraceIDOrNew(id); got == id || !re.MatchString(got) {
			t.Errorf("TraceIDOrNew(%q) = %q, want a fresh 32-hex id", id, got)
		}
	}
}

func TestTraceContext(t *testing.T) {
	base := context.Background()
	if TraceFromContext(base) != "" || TraceFromContext(nil) != "" { //nolint:staticcheck // nil ctx is part of the contract
		t.Error("absent trace must read as empty, nil ctx included")
	}
	if got := TraceFromContext(ContextWithTrace(base, "abcd1234")); got != "abcd1234" {
		t.Errorf("round trip = %q", got)
	}
	if ctx := ContextWithTrace(base, "bad id"); ctx != base {
		t.Error("an invalid id must leave the context unchanged")
	}
	withID := ContextWithTrace(base, "abcd1234")
	if got := TraceFromContext(ContextWithTrace(withID, "bad")); got != "abcd1234" {
		t.Errorf("invalid id replaced an existing trace: %q", got)
	}
}

func TestOnBox(t *testing.T) {
	hosts := map[string]bool{
		"localhost": true, "LocalHost": true, "127.0.0.1": true, "127.5.6.7": true, "::1": true, "::ffff:127.0.0.1": true,
		"": false, "192.168.1.5": false, "10.0.0.1": false, "api.example.com": false, "localhost.example.com": false, "128.0.0.1": false, "::2": false,
	}
	for h, want := range hosts {
		if got := OnBoxHost(h); got != want {
			t.Errorf("OnBoxHost(%q) = %v, want %v", h, got, want)
		}
	}
	urls := map[string]bool{
		"http://127.0.0.1:8080/v1": true, "http://localhost/v1": true, "http://[::1]:9/x": true, "unix:///tmp/a.sock": true,
		"https://api.example.com/v1": false, "http://192.168.64.1:8180": false, "http://127.0.0.1.example.com/": false,
	}
	for raw, want := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := OnBoxURL(u); got != want {
			t.Errorf("OnBoxURL(%q) = %v, want %v", raw, got, want)
		}
	}
	if OnBoxURL(nil) {
		t.Error("OnBoxURL(nil) must be false")
	}
}

// Criterion 10: no credential-bearing part of a URL reaches a log line.
func TestSafeURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.example.com/v1":                     "https://api.example.com/v1",
		"http://user:pw@host.example:8080/v1/x":          "http://host.example:8080/v1/x",
		"https://tok@host.example/v1?api_key=SECRET&b=1": "https://host.example/v1",
		"https://host.example/v1#frag":                   "https://host.example/v1",
		"https://user:pw@host.example?key=SECRET#f":      "https://host.example",
		"":                                     "<invalid url>",
		"not a url":                            "<invalid url>",
		"http://[::1":                          "<invalid url>",
		"http://user:pw@%zz/path?token=SECRET": "<invalid url>",
	} {
		got := SafeURL(in)
		if got != want {
			t.Errorf("SafeURL(%q) = %q, want %q", in, got, want)
		}
		for _, leak := range []string{"SECRET", "pw@", "user", "tok@"} {
			if strings.Contains(got, leak) {
				t.Errorf("SafeURL(%q) leaked %q: %q", in, leak, got)
			}
		}
	}
}
