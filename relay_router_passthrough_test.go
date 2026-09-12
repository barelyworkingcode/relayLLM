package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"relayllm/internal/config"
	"relayllm/internal/testutil"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newPassthroughRouter(t *testing.T, cfg map[string]config.PassthroughConfig) *httptest.Server {
	t.Helper()
	r := NewRelayRouter(":0", nil, nil, nil)
	r.setPassthrough(cfg)
	srv := httptest.NewServer(r.server.Handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestNewPassthroughProxy_Validation(t *testing.T) {
	cases := []struct {
		name, entry, upstream string
		ok                    bool
	}{
		{"https upstream", "chatgpt", "https://chatgpt.com/backend-api", true},
		{"loopback http upstream", "local", "http://127.0.0.1:9000", true},
		{"reserved: api", "api", "https://api.openai.com", false},
		{"reserved: v1", "v1", "https://api.openai.com", false},
		{"multi-segment name", "a/b", "https://api.openai.com", false},
		{"empty upstream", "openai", "", false},
		{"non-http scheme", "openai", "ftp://api.openai.com", false},
		{"plaintext off-box upstream", "openai", "http://api.openai.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := newPassthroughProxy(tc.entry, config.PassthroughConfig{Upstream: tc.upstream})
			if (err == nil) != tc.ok {
				t.Errorf("newPassthroughProxy(%q, %q) err = %v, want ok=%v", tc.entry, tc.upstream, err, tc.ok)
			}
		})
	}
}

// The client's credential and headers reach the upstream untouched, the
// path lands under the upstream's base path, and the reply comes back as-is.
func TestPassthrough_ForwardsCredentialUnderUpstreamBasePath(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotAccount, gotXFF string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("Chatgpt-Account-Id")
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "req-123")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer upstream.Close()

	srv := newPassthroughRouter(t, map[string]config.PassthroughConfig{
		"chatgpt": {Upstream: upstream.URL + "/backend-api"},
	})

	sent := []byte(`{"model":"gpt-5.5","stream":true,"input":[]}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/chatgpt/codex/responses?trace=1", bytes.NewReader(sent))
	req.Header.Set("Authorization", "Bearer chatgpt-oauth-token")
	req.Header.Set("Chatgpt-Account-Id", "acct-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Request-Id") != "req-123" {
		t.Errorf("response status=%d X-Request-Id=%q, want 200 and upstream header relayed", resp.StatusCode, resp.Header.Get("X-Request-Id"))
	}
	if string(body) != "data: {\"type\":\"response.completed\"}\n\n" {
		t.Errorf("response body = %q, want upstream bytes verbatim", body)
	}
	if gotPath != "/backend-api/codex/responses" || gotQuery != "trace=1" {
		t.Errorf("upstream saw %s?%s, want /backend-api/codex/responses?trace=1", gotPath, gotQuery)
	}
	if gotAuth != "Bearer chatgpt-oauth-token" || gotAccount != "acct-1" {
		t.Errorf("upstream Authorization=%q Chatgpt-Account-Id=%q, want both forwarded verbatim", gotAuth, gotAccount)
	}
	if gotXFF != "" {
		t.Errorf("upstream X-Forwarded-For = %q, want absent", gotXFF)
	}
	if !bytes.Equal(gotBody, sent) {
		t.Errorf("upstream body = %s, want byte-identical %s", gotBody, sent)
	}
}

// OpenAI model ids never dispatch by model field. A request on the router's
// own routes stays local even when an OpenAI passthrough is configured, so a
// mistyped local model name can't leave the box.
func TestPassthrough_LocalRoutesNeverReachUpstream(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer upstream.Close()

	srv := newPassthroughRouter(t, map[string]config.PassthroughConfig{"openai": {Upstream: upstream.URL}})

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"gpt-5.5"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("local route status = %d, want the router's own 400", resp.StatusCode)
	}
	if hits.Load() != 0 {
		t.Errorf("upstream hit %d times, want 0", hits.Load())
	}
}

// Invalid entries are skipped, not fatal. "api" in particular must not reach
// ServeMux, where a duplicate "/api/" pattern panics.
func TestPassthrough_InvalidEntriesSkippedValidOneMounted(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer upstream.Close()

	srv := newPassthroughRouter(t, map[string]config.PassthroughConfig{
		"api":   {Upstream: upstream.URL},
		"plain": {Upstream: "http://api.openai.com"},
		"good":  {Upstream: upstream.URL},
	})

	resp, err := http.Get(srv.URL + "/plain/v1/models")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("/plain/ status = %d, want the router's own 400 (entry skipped)", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/good/v1/models")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || hits.Load() != 1 {
		t.Errorf("/good/ status = %d, upstream hits = %d, want 200 and 1", resp.StatusCode, hits.Load())
	}
}

// OMP's codex transport prefers WebSocket. The upgrade must reach the
// upstream with the credential, frames must flow both ways, and the
// connection must be metered and read as idle, not stalled.
func TestPassthrough_WebSocketUpgradeRelayedAndMetered(t *testing.T) {
	var gotAuth, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("upstream hijack: %v", err)
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		frame := make([]byte, 5)
		if _, err := io.ReadFull(brw, frame); err != nil {
			return
		}
		conn.Write(frame)
		io.Copy(io.Discard, conn) // hold until the client hangs up
	}))
	defer upstream.Close()

	r := NewRelayRouter(":0", nil, nil, nil)
	r.setPassthrough(map[string]config.PassthroughConfig{"chatgpt": {Upstream: upstream.URL + "/backend-api"}})
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	client, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial router: %v", err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(client, "GET /chatgpt/codex/responses HTTP/1.1\r\n"+
		"Host: router\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+
		"Authorization: Bearer chatgpt-oauth-token\r\n\r\n")

	br := bufio.NewReader(client)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d, want 101", resp.StatusCode)
	}
	io.WriteString(client, "ping!")
	echo := make([]byte, 5)
	if _, err := io.ReadFull(br, echo); err != nil || string(echo) != "ping!" {
		t.Fatalf("echo = %q, err = %v, want ping!", echo, err)
	}

	if gotAuth != "Bearer chatgpt-oauth-token" || gotPath != "/backend-api/codex/responses" {
		t.Errorf("upstream saw Authorization=%q path=%q", gotAuth, gotPath)
	}

	var row ProxyConnInfo
	testutil.WaitFor(t, 2*time.Second, func() bool {
		active, _, _ := r.Metrics().Snapshot()
		if len(active) != 1 {
			return false
		}
		row = active[0]
		return row.BytesIn >= 5 && row.BytesOut >= 5
	})
	if row.Status != http.StatusSwitchingProtocols || row.TargetKind != "passthrough" || row.Target != "chatgpt" {
		t.Errorf("row status=%d kind=%q target=%q, want 101 passthrough chatgpt", row.Status, row.TargetKind, row.Target)
	}
	if row.State == connStateStalled || row.State == connStateQuiet {
		t.Errorf("row state = %q, want active or idle", row.State)
	}

	client.Close()
	testutil.WaitFor(t, 2*time.Second, func() bool {
		active, _, recent := r.Metrics().Snapshot()
		return len(active) == 0 && len(recent) == 1
	})
}

func TestStartRelayRouter_PassthroughOnlyConfigStillStarts(t *testing.T) {
	router, err := StartRelayRouter([]string{"127.0.0.1:0"}, nil, nil, nil, &config.RouterConfig{
		Passthrough: map[string]config.PassthroughConfig{"chatgpt": {Upstream: "https://chatgpt.com/backend-api"}},
	}, "", "")
	if err != nil {
		t.Fatalf("StartRelayRouter: %v", err)
	}
	if router == nil {
		t.Fatal("expected a router when router.passthrough is configured, even with no local backends")
	}
	t.Cleanup(func() { router.Close() })
}
