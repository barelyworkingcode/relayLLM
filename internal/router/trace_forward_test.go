package router

// Coverage for relayLLM#25 L2 in the router: the byte-for-byte proxies
// (router.passthrough and the Anthropic passthrough) forward relayLLM's own
// validated trace ID to an on-box upstream and never a trace ID to an off-box
// one; and no endpoint URL log line or error carries credentials.
//
// No hermetic door reaches a real off-box host, so off-box cases give the
// real proxy a recording RoundTripper in place of its network transport and
// drive it through the real router listener with an https upstream URL on a
// non-loopback name. The Rewrite and the trace middleware are the real ones.

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"relayllm/internal/config"
	"relayllm/internal/logging"
	regpkg "relayllm/internal/registry"
)

type seenReq struct {
	trace, auth string
	hasTrace    bool
}

type recorder struct {
	mu   sync.Mutex
	seen []seenReq
}

func (r *recorder) record(h http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, has := h[http.CanonicalHeaderKey(logging.TraceHeader)]
	r.seen = append(r.seen, seenReq{trace: h.Get(logging.TraceHeader), auth: h.Get("Authorization"), hasTrace: has})
}

func (r *recorder) single(t *testing.T) seenReq {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(r.seen))
	}
	return r.seen[0]
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (r *recorder) transport() http.RoundTripper {
	return rtFunc(func(req *http.Request) (*http.Response, error) {
		r.record(req.Header)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
	})
}

func onBoxUpstream(t *testing.T) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		rec.record(r.Header)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{}")
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// door is one byte-for-byte proxy route plus the way to build a router for
// it against a given upstream URL.
type door struct {
	name, path, body string
	setup            func(r *RelayRouter, upstream string)
}

var forwardDoors = []door{
	{"router.passthrough", "/chatgpt/codex/responses", `{"input":[]}`, func(r *RelayRouter, up string) {
		r.setPassthrough(map[string]config.PassthroughConfig{"chatgpt": {Upstream: up}})
	}},
	{"anthropic unmapped model", "/v1/messages", `{"model":"claude-acme-1","messages":[]}`, func(r *RelayRouter, up string) {
		r.setAnthropic(&config.AnthropicRouterConfig{Upstream: up})
	}},
	{"anthropic /api/", "/api/hello", `{}`, func(r *RelayRouter, up string) {
		r.setAnthropic(&config.AnthropicRouterConfig{Upstream: up})
	}},
}

const clientAuth = "Bearer client-oauth-token-1"

// Criteria 5, 6: on-box, the router's own validated ID arrives (a valid
// inbound ID is kept; an absent or invalid one is replaced by a fresh one that
// is also the logged one), and the client's credential is untouched.
func TestForward_OnBoxUpstreamGetsRouterTraceID(t *testing.T) {
	const valid = "Acme-trace_0123456789"
	inbounds := []struct{ name, id string }{
		{"valid", valid},
		{"absent", ""},
		{"invalid", "REJECTED value;with junk"},
	}
	for _, d := range forwardDoors {
		for _, in := range inbounds {
			t.Run(d.name+"/"+in.name, func(t *testing.T) {
				logs := captureLogs(t)
				up, rec := onBoxUpstream(t)
				r := NewRelayRouter(":0", nil, nil, nil)
				d.setup(r, up.URL)
				base := serveTCP(t, r)

				hdr := map[string]string{"Authorization": clientAuth}
				if in.id != "" {
					hdr["X-Trace-Id"] = in.id
				}
				post(t, base+d.path, d.body, hdr)

				got := rec.single(t)
				if got.auth != clientAuth {
					t.Errorf("upstream Authorization = %q, want %q", got.auth, clientAuth)
				}
				if in.name == "valid" {
					if got.trace != valid {
						t.Errorf("upstream trace = %q, want %q", got.trace, valid)
					}
				} else if !hex32.MatchString(got.trace) {
					t.Errorf("upstream trace = %q, want a fresh 32-hex ID", got.trace)
				}
				if line := logs.waitOneRequestLine(t); line["trace_id"] != got.trace {
					t.Errorf("logged trace_id %v != upstream trace %q", line["trace_id"], got.trace)
				}
			})
		}
	}
}

// Criterion 7: an off-box (hosted) upstream never sees a trace header,
// whether the client sent one or the router made one. The client's
// credential still arrives untouched.
func TestForward_OffBoxUpstreamNeverGetsTraceID(t *testing.T) {
	inbounds := map[string]string{"client sent valid": "Acme-trace_0123456789", "client sent none": "", "client sent invalid": "bad id;junk"}
	for _, d := range forwardDoors {
		for name, id := range inbounds {
			t.Run(d.name+"/"+name, func(t *testing.T) {
				captureLogs(t)
				rec := &recorder{}
				r := NewRelayRouter(":0", nil, nil, nil)
				if d.name != "router.passthrough" {
					d.setup(r, "https://api.acme.example/backend")
					r.anthropic.transport = rec.transport()
				} else {
					// router.passthrough builds its proxy inside setPassthrough,
					// so mount the same entry by hand with a recording transport.
					proxy, _, err := newPassthroughProxy("chatgpt", config.PassthroughConfig{Upstream: "https://api.acme.example/backend"})
					if err != nil {
						t.Fatal(err)
					}
					proxy.Transport = rec.transport()
					r.mux.HandleFunc("/chatgpt/", proxy.ServeHTTP)
				}
				base := serveTCP(t, r)

				hdr := map[string]string{"Authorization": clientAuth}
				if id != "" {
					hdr["X-Trace-Id"] = id
				}
				resp := post(t, base+d.path, d.body, hdr)
				if resp.StatusCode != 200 {
					t.Fatalf("status = %d, want 200 via the recording transport", resp.StatusCode)
				}
				got := rec.single(t)
				if got.hasTrace {
					t.Errorf("off-box upstream saw X-Trace-Id %q", got.trace)
				}
				if got.auth != clientAuth {
					t.Errorf("upstream Authorization = %q, want %q", got.auth, clientAuth)
				}
			})
		}
	}
}

// Criterion 6: a WebSocket upgrade through router.passthrough carries the
// router's trace ID to an on-box upstream.
func TestForward_PassthroughWebSocketUpgradeCarriesTraceID(t *testing.T) {
	captureLogs(t)
	const id = "Acme-trace_0123456789"
	seen := make(chan seenReq, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{}
		rec.record(r.Header)
		seen <- rec.seen[0]
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
	}))
	defer up.Close()

	r := NewRelayRouter(":0", nil, nil, nil)
	r.setPassthrough(map[string]config.PassthroughConfig{"chatgpt": {Upstream: up.URL}})
	serveTCP(t, r)

	client, err := net.Dial("tcp", r.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(client, "GET /chatgpt/codex/responses HTTP/1.1\r\n"+
		"Host: router\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+
		"Authorization: "+clientAuth+"\r\nX-Trace-Id: "+id+"\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: status/err = %v / %v, want 101", resp, err)
	}
	got := <-seen
	if got.trace != id || got.auth != clientAuth {
		t.Errorf("upstream saw trace=%q auth=%q, want %q and the client credential", got.trace, got.auth, id)
	}
}

// Criterion 10: an endpoint URL with userinfo and a query key keeps both out
// of every log line at the router's own log sites. The host stays visible
// where a URL is logged at all.
func TestRouterLogs_EndpointURLCredentialsNeverLogged(t *testing.T) {
	const (
		pw  = "pw-Zx81qSecret"
		key = "key-Zx81qSecret"
	)
	assertClean := func(t *testing.T, logs string) {
		t.Helper()
		for _, s := range []string{pw, key} {
			if strings.Contains(logs, s) {
				t.Errorf("log leaks %q: %s", s, logs)
			}
		}
	}

	t.Run("passthrough mount", func(t *testing.T) {
		logs := captureLogs(t)
		r := NewRelayRouter(":0", nil, nil, nil)
		r.setPassthrough(map[string]config.PassthroughConfig{"chatgpt": {Upstream: "https://svc:" + pw + "@api.acme.example/backend?k=" + key}})
		if len(r.passthroughNames) != 1 {
			t.Fatalf("entry did not mount")
		}
		assertClean(t, logs.all())
		if !strings.Contains(logs.all(), "api.acme.example") {
			t.Errorf("mount line no longer names the host: %s", logs.all())
		}
	})

	t.Run("passthrough entry rejected", func(t *testing.T) {
		logs := captureLogs(t)
		r := NewRelayRouter(":0", nil, nil, nil)
		r.setPassthrough(map[string]config.PassthroughConfig{"chatgpt": {Upstream: "http://svc:" + pw + "@api.acme.example/backend?k=" + key}})
		if len(r.passthroughNames) != 0 {
			t.Fatalf("plain http off-box entry must not mount")
		}
		if logs.all() == "" {
			t.Fatal("no log line for a rejected entry")
		}
		assertClean(t, logs.all())
	})

	t.Run("anthropic upstream invalid", func(t *testing.T) {
		logs := captureLogs(t)
		r := NewRelayRouter(":0", nil, nil, nil)
		r.setAnthropic(&config.AnthropicRouterConfig{Upstream: "http://svc:" + pw + "@api.acme.example:80x/?k=" + key})
		if r.anthropic != nil {
			t.Fatal("unparseable upstream must disable anthropic compatibility")
		}
		if logs.all() == "" {
			t.Fatal("no log line for an invalid upstream")
		}
		assertClean(t, logs.all())
	})

	t.Run("audio route bad baseURL", func(t *testing.T) {
		logs := captureLogs(t)
		ep := config.OpenAIEndpoint{Name: "acme", BaseURL: "http://svc:" + pw + "@api.acme.example:80x/v1?k=" + key}
		reg := regpkg.NewProxyRegistry(&config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{ep}})
		reg.SetStatusForTest(ep, true)
		r := NewRelayRouter(":0", nil, reg, nil)
		base := serveTCP(t, r)

		body, ct := audioForm(t, "acme/whisper", []byte("RIFFdata"), nil)
		resp := postForm(t, base+"/v1/audio/transcriptions", body, ct)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", resp.StatusCode)
		}
		got, _ := io.ReadAll(resp.Body)
		assertClean(t, string(got))
		assertClean(t, logs.all())
	})
}
