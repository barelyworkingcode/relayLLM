package router

// Coverage for relayLLM#25 L1 in the router: trace-ID acceptance and
// forwarding, and exactly one model.request line per proxied request, driven
// through the real doors (Listen+Serve on loopback, and router.sock).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"relayllm/internal/config"
	"relayllm/internal/logging"
	"relayllm/internal/relay"
	"relayllm/internal/servermanager"
	"relayllm/internal/testutil"
)

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// requestLines returns the parsed model.request lines written so far.
func (l *logBuf) requestLines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(l.all(), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if m["op"] == "model.request" {
			out = append(out, m)
		}
	}
	return out
}

// waitOneRequestLine waits for the first line, then gives a duplicate time to
// show up, and returns the single line.
func (l *logBuf) waitOneRequestLine(t *testing.T) map[string]any {
	t.Helper()
	testutil.WaitFor(t, 3*time.Second, func() bool { return len(l.requestLines(t)) >= 1 })
	time.Sleep(100 * time.Millisecond)
	lines := l.requestLines(t)
	if len(lines) != 1 {
		t.Fatalf("got %d model.request lines, want exactly 1: %v", len(lines), lines)
	}
	return lines[0]
}

func captureLogs(t *testing.T) *logBuf {
	t.Helper()
	buf := &logBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler(buf, logging.Options{Getenv: func(string) string { return "" }})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// serveTCP starts r on a loopback port through the real Listen+Serve door.
func serveTCP(t *testing.T, r *RelayRouter) string {
	t.Helper()
	if err := r.Listen([]string{"127.0.0.1:0"}); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	r.Serve()
	t.Cleanup(func() { r.Close() })
	return "http://" + r.Addr()
}

// chatBackend is an on-box fake upstream recording what reached it.
type chatBackend struct {
	*httptest.Server
	mu      sync.Mutex
	traces  []string
	hits    int
	lastRaw []byte
}

func newChatBackend(t *testing.T) *chatBackend {
	t.Helper()
	b := &chatBackend{}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.traces = append(b.traces, r.Header.Get(logging.TraceHeader))
		b.hits++
		b.lastRaw = raw
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp","choices":[]}`)
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *chatBackend) seenTraces() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.traces...)
}

// managedRouter returns a router whose alias "m1" is a ready managed server
// backed by the given on-box fake.
func managedRouter(t *testing.T, backend *httptest.Server) *RelayRouter {
	t.Helper()
	mgr := servermanager.NewServerManager(servermanager.LlamaProfile, &config.ServerConfig{
		Models: []config.ServerModelConfig{{Alias: "m1", Args: map[string]any{"model": "/fake"}}},
	}, "")
	mgr.InjectReadyInstanceForTest("m1", backend.Listener.Addr().(*net.TCPAddr).Port, 0)
	return NewRelayRouter(":0", []*servermanager.ServerManager{mgr}, nil, nil)
}

func post(t *testing.T, url string, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// Criterion 5: a valid inbound X-Trace-Id is kept, logged and forwarded
// on-box.
func TestTrace_ValidInboundIDKeptLoggedAndForwarded(t *testing.T) {
	logs := captureLogs(t)
	backend := newChatBackend(t)
	base := serveTCP(t, managedRouter(t, backend.Server))

	const id = "Acme-trace_0123456789"
	post(t, base+"/v1/chat/completions", `{"model":"m1"}`, map[string]string{"X-Trace-Id": id})

	if got := backend.seenTraces(); len(got) != 1 || got[0] != id {
		t.Errorf("backend saw trace headers %q, want [%s]", got, id)
	}
	if line := logs.waitOneRequestLine(t); line["trace_id"] != id {
		t.Errorf("log trace_id = %v, want %s", line["trace_id"], id)
	}
}

// Criterion 5: an absent or invalid ID is replaced with a fresh one, the
// same one is logged and forwarded, and the rejected value is never logged.
func TestTrace_AbsentOrInvalidInboundIDReplaced(t *testing.T) {
	cases := map[string]string{
		"absent":    "",
		"too short": "abc",
		"spaces":    "REJECTED value with spaces",
		"too long":  "REJECTED" + strings.Repeat("x", 80),
		"bad chars": "REJECTED;drop=table",
	}
	for name, inbound := range cases {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			backend := newChatBackend(t)
			base := serveTCP(t, managedRouter(t, backend.Server))

			hdr := map[string]string{}
			if inbound != "" {
				hdr["X-Trace-Id"] = inbound
			}
			post(t, base+"/v1/chat/completions", `{"model":"m1"}`, hdr)

			line := logs.waitOneRequestLine(t)
			id, _ := line["trace_id"].(string)
			if !hex32.MatchString(id) {
				t.Fatalf("log trace_id = %q, want 32 lowercase hex", id)
			}
			if got := backend.seenTraces(); len(got) != 1 || got[0] != id {
				t.Errorf("backend saw %q, want the generated %s", got, id)
			}
			if inbound != "" && strings.Contains(logs.all(), "REJECTED") {
				t.Errorf("rejected inbound value was logged: %s", logs.all())
			}
		})
	}
}

// Criterion 9 and the status mapping: one line per request, with the level,
// status and error the contract names.
func TestModelRequestLine_OnePerRequestWithMapping(t *testing.T) {
	type want struct {
		level, status, errText string
		httpStatus             float64
		target                 string
	}
	cases := []struct {
		name  string
		setup func(t *testing.T) (base string)
		do    func(base string) *http.Response
		want  want
	}{
		{
			name: "managed 200",
			setup: func(t *testing.T) string {
				return serveTCP(t, managedRouter(t, newChatBackend(t).Server))
			},
			do: func(base string) *http.Response {
				return post(t, base+"/v1/chat/completions", `{"model":"m1"}`, nil)
			},
			want: want{"info", "ok", "", 200, "m1"},
		},
		{
			name:  "unknown model 400",
			setup: func(t *testing.T) string { return serveTCP(t, NewRelayRouter(":0", nil, nil, nil)) },
			do: func(base string) *http.Response {
				return post(t, base+"/v1/chat/completions", `{"model":"nope"}`, nil)
			},
			want: want{"warn", "error", "http_400", 400, ""},
		},
		{
			name: "managed launch failure 502",
			setup: func(t *testing.T) string {
				mgr := servermanager.NewServerManager(servermanager.LlamaProfile, &config.ServerConfig{
					BinaryPath: "/nonexistent/llama-server-for-test",
					Models:     []config.ServerModelConfig{{Alias: "m1", Args: map[string]any{"model": "/fake"}}},
				}, "")
				return serveTCP(t, NewRelayRouter(":0", []*servermanager.ServerManager{mgr}, nil, nil))
			},
			do: func(base string) *http.Response {
				return post(t, base+"/v1/chat/completions", `{"model":"m1"}`, nil)
			},
			want: want{"error", "error", "http_502", 502, ""},
		},
		{
			name: "router key missing 401 is denied",
			setup: func(t *testing.T) string {
				r := NewRelayRouter(":0", nil, nil, nil)
				r.EnableStandaloneRouterKeys(RouterKeysPath(t.TempDir()))
				return serveTCP(t, r)
			},
			do: func(base string) *http.Response {
				resp, err := http.Get(base + "/health")
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				return resp
			},
			want: want{"warn", "denied", "http_401", 401, ""},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logs := captureLogs(t)
			base := c.setup(t)
			resp := c.do(base)
			line := logs.waitOneRequestLine(t)
			if line["level"] != c.want.level || line["status"] != c.want.status || line["error"] != c.want.errText {
				t.Errorf("level/status/error = %v/%v/%v, want %s/%s/%s", line["level"], line["status"], line["error"], c.want.level, c.want.status, c.want.errText)
			}
			if line["http_status"] != c.want.httpStatus || float64(resp.StatusCode) != c.want.httpStatus {
				t.Errorf("http_status = %v (client saw %d), want %v", line["http_status"], resp.StatusCode, c.want.httpStatus)
			}
			if line["transport"] != "tcp" || line["method"] == "" || line["msg"] != "model request" {
				t.Errorf("line = %v", line)
			}
			if d, ok := line["duration_ms"].(float64); !ok || d < 0 {
				t.Errorf("duration_ms = %v", line["duration_ms"])
			}
			if got, _ := line["target"].(string); got != c.want.target {
				t.Errorf("target = %q, want %q", got, c.want.target)
			}
		})
	}
}

// Catalog and health polls would drown the log; successful ones write none.
func TestModelRequestLine_SuccessfulPollsWriteNothing(t *testing.T) {
	logs := captureLogs(t)
	base := serveTCP(t, managedRouter(t, newChatBackend(t).Server))

	for _, m := range []string{http.MethodGet, http.MethodHead} {
		for _, p := range []string{"/health", "/v1/models", "/models"} {
			req, _ := http.NewRequest(m, base+p, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode >= 400 {
				t.Fatalf("%s %s = %d", m, p, resp.StatusCode)
			}
		}
	}
	// A real request after the polls proves the log is live and in order.
	post(t, base+"/v1/chat/completions", `{"model":"m1"}`, nil)
	line := logs.waitOneRequestLine(t)
	if line["path"] != "/v1/chat/completions" {
		t.Errorf("the only line should be the chat request, got %v", line)
	}
}

// Criterion 12: no prompt, credential or query string reaches the log.
func TestModelRequestLine_NeverCarriesPromptCredentialsOrQuery(t *testing.T) {
	logs := captureLogs(t)
	base := serveTCP(t, managedRouter(t, newChatBackend(t).Server))

	post(t, base+"/v1/chat/completions?api_key=CANARY-QUERY",
		`{"model":"m1","messages":[{"role":"user","content":"CANARY-PROMPT"}]}`,
		map[string]string{"Authorization": "Bearer CANARY-TOKEN", "X-Api-Key": "CANARY-APIKEY", "X-Relay-Key": "CANARY-RELAYKEY"})
	post(t, base+"/v1/chat/completions?api_key=CANARY-QUERY",
		`{"model":"nope","messages":[{"role":"user","content":"CANARY-PROMPT"}]}`,
		map[string]string{"Authorization": "Bearer CANARY-TOKEN"})
	testutil.WaitFor(t, 3*time.Second, func() bool { return len(logs.requestLines(t)) >= 2 })
	time.Sleep(50 * time.Millisecond)

	out := logs.all()
	for _, canary := range []string{"CANARY-PROMPT", "CANARY-TOKEN", "CANARY-QUERY", "CANARY-APIKEY", "CANARY-RELAYKEY"} {
		if strings.Contains(out, canary) {
			t.Errorf("log output contains %s:\n%s", canary, out)
		}
	}
	for _, l := range logs.requestLines(t) {
		if l["path"] != "/v1/chat/completions" {
			t.Errorf("path = %v, want the bare path", l["path"])
		}
	}
}

// Streaming must still flush through the middleware, and the stream is one
// line.
func TestModelRequestLine_SSEStreamsLiveAndLogsOnce(t *testing.T) {
	logs := captureLogs(t)
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		io.WriteString(w, "data: second\n\n")
	}))
	defer backend.Close()
	base := serveTCP(t, managedRouter(t, backend))

	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m1","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	first := make(chan string, 1)
	go func() { l, _ := br.ReadString('\n'); first <- l }()
	select {
	case l := <-first:
		if l != "data: first\n" {
			t.Fatalf("first chunk = %q", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first SSE chunk was not flushed while the upstream was still open")
	}
	close(release)
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "second") {
		t.Errorf("rest of stream = %q", rest)
	}
	if line := logs.waitOneRequestLine(t); line["http_status"] != 200.0 || line["status"] != "ok" {
		t.Errorf("line = %v", line)
	}
}

// A websocket upgrade through a passthrough must still work and logs one
// line, 101, when the connection ends.
func TestModelRequestLine_WebSocketPassthroughUpgradeLogsOnce101(t *testing.T) {
	logs := captureLogs(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		io.Copy(io.Discard, conn)
	}))
	defer upstream.Close()
	r := NewRelayRouter(":0", nil, nil, nil)
	r.setPassthrough(map[string]config.PassthroughConfig{"chatgpt": {Upstream: upstream.URL + "/backend-api"}})
	base := serveTCP(t, r)

	client, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(client, "GET /chatgpt/codex/responses HTTP/1.1\r\nHost: router\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nAuthorization: Bearer CANARY-TOKEN\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v, %v", resp, err)
	}
	client.Close()

	line := logs.waitOneRequestLine(t)
	if line["http_status"] != 101.0 || line["status"] != "ok" || line["path"] != "/chatgpt/codex/responses" {
		t.Errorf("line = %v", line)
	}
	if strings.Contains(logs.all(), "CANARY-TOKEN") {
		t.Error("credential logged")
	}
}

// The Anthropic redirect re-enters the proxy internally; it is still one
// request and one line, under the caller's trace.
func TestModelRequestLine_AnthropicRedirectWritesExactlyOneLine(t *testing.T) {
	logs := captureLogs(t)
	var hdr http.Header
	var body []byte
	upstream := openaiSSEUpstream(t, &hdr, &body)
	r := anthropicRedirectFixture(t, "local-model", upstream, map[string]string{"claude-haiku-4-5": "local-model"})
	base := serveTCP(t, r)

	const id = "redirect-trace-0001"
	post(t, base+"/v1/messages", `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"CANARY-PROMPT"}],"stream":true}`,
		map[string]string{"X-Trace-Id": id, "Authorization": "Bearer CANARY-TOKEN"})

	line := logs.waitOneRequestLine(t)
	if line["trace_id"] != id || line["path"] != "/v1/messages" {
		t.Errorf("line = %v", line)
	}
	if hdr.Get("X-Trace-Id") != id {
		t.Errorf("local backend saw X-Trace-Id %q, want %s", hdr.Get("X-Trace-Id"), id)
	}
	if out := logs.all(); strings.Contains(out, "CANARY") {
		t.Errorf("canary leaked into log: %s", out)
	}
}

// router.sock is the door relay uses; it must get a trace and a line too.
func TestTrace_RouterSockRequestGetsTraceAndLine(t *testing.T) {
	selfHello(t)
	logs := captureLogs(t)
	r := NewRelayRouter(":0", nil, nil, nil)
	sock := shortSocketPath(t)
	if err := r.ListenSocket(sock, relay.RelayIdentity); err != nil {
		t.Fatalf("ListenSocket: %v", err)
	}
	defer r.Close()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return net.Dial("unix", sock) },
	}}

	t.Run("generated", func(t *testing.T) {
		resp, err := client.Post("http://unix/v1/chat/completions", "application/json", strings.NewReader(`{"model":"nope"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		line := logs.waitOneRequestLine(t)
		if id, _ := line["trace_id"].(string); !hex32.MatchString(id) {
			t.Errorf("trace_id = %v, want generated 32-hex", line["trace_id"])
		}
		if line["transport"] != "socket" || line["http_status"] != 400.0 {
			t.Errorf("line = %v", line)
		}
	})
	t.Run("inbound kept", func(t *testing.T) {
		before := len(logs.requestLines(t))
		req, _ := http.NewRequest(http.MethodPost, "http://unix/v1/chat/completions", strings.NewReader(`{"model":"nope"}`))
		req.Header.Set("X-Trace-Id", "from-relay-0001")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		testutil.WaitFor(t, 3*time.Second, func() bool { return len(logs.requestLines(t)) > before })
		lines := logs.requestLines(t)
		if got := lines[len(lines)-1]["trace_id"]; got != "from-relay-0001" {
			t.Errorf("trace_id = %v", got)
		}
	})
}

// Criterion 7: the Director forwards the context's trace ID to an on-box
// target only. A hosted target gets none, and a caller-supplied header never
// passes through. Driven at the Director because a hermetic test cannot
// stand up a hosted provider.
func TestUpstreamProxy_TraceHeaderOnlyToOnBoxTargets(t *testing.T) {
	const id = "ctx-trace-0001"
	cases := []struct {
		name, target, inbound string
		ctxID                 string
		want                  string
	}{
		{"loopback gets context id", "http://127.0.0.1:9000/v1", "", id, id},
		{"localhost gets context id", "http://localhost:9000/v1", "", id, id},
		{"loopback overwrites caller value", "http://127.0.0.1:9000/v1", "caller-forged-1", id, id},
		{"hosted gets none", "https://api.example.com/v1", "", id, ""},
		{"hosted drops caller value", "https://api.example.com/v1", "caller-forged-1", id, ""},
		{"lan host gets none", "http://192.168.64.1:8180/v1", "", id, ""},
		{"loopback without context id drops caller value", "http://127.0.0.1:9000/v1", "caller-forged-1", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target, _ := url.Parse(c.target)
			proxy := newUpstreamProxy(target, []byte(`{}`), "", "test", "test", nil)
			ctx := context.Background()
			if c.ctxID != "" {
				ctx = logging.ContextWithTrace(ctx, c.ctxID)
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://router/v1/chat/completions", nil)
			if c.inbound != "" {
				req.Header.Set("X-Trace-Id", c.inbound)
			}
			proxy.Director(req)
			if got := req.Header.Get("X-Trace-Id"); got != c.want {
				t.Errorf("X-Trace-Id = %q, want %q", got, c.want)
			}
		})
	}
}
