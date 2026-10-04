package relay_test

// Coverage for relayLLM#25 L2 on the bridge: the request envelope carries the
// caller's valid trace ID as "trace_id", and has no such key otherwise.

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"relayllm/internal/logging"
	"relayllm/internal/relay"
	"relayllm/internal/testutil"
)

// rawBridge answers one request per connection and keeps the raw line it read,
// so a test can see which keys were really on the wire.
func rawBridge(t *testing.T) (sock string, lines chan string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rb")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock = filepath.Join(dir, "r.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	lines = make(chan string, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				if sc.Scan() {
					lines <- sc.Text()
					c.Write([]byte(`{"type":"OK"}` + "\n"))
				}
			}()
		}
	}()
	return sock, lines
}

func launchedAgainstRaw(t *testing.T) chan string {
	t.Helper()
	fb := testutil.NewFakeBridge(t)
	testutil.LaunchViaBridge(t, fb, "relay-llm")
	sock, lines := rawBridge(t)
	t.Setenv(relay.EnvBridgeSocket, sock)
	return lines
}

func wireKeys(t *testing.T, line string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("wire line is not JSON: %q", line)
	}
	return m
}

// Criterion 6: the bridge request carries a valid trace ID.
func TestSendBridgeRequestContext_TracedContextSendsTraceID(t *testing.T) {
	lines := launchedAgainstRaw(t)
	const id = "Acme-trace_0123456789"
	ctx := logging.ContextWithTrace(context.Background(), id)
	if _, err := relay.SendBridgeRequestContext(ctx, "ListProjects", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("send: %v", err)
	}
	m := wireKeys(t, <-lines)
	if string(m["trace_id"]) != `"`+id+`"` {
		t.Errorf("trace_id on the wire = %s, want %q", m["trace_id"], id)
	}
}

// Criterion 6: with no valid ID the key is absent, not empty, and the old
// entry point is unchanged.
func TestBridgeRequest_NoTraceIDKeyWithoutValidTrace(t *testing.T) {
	cases := map[string]func() error{
		"untraced context": func() error {
			_, err := relay.SendBridgeRequestContext(context.Background(), "ListProjects", json.RawMessage(`{}`))
			return err
		},
		"invalid id in context": func() error {
			ctx := logging.ContextWithTrace(context.Background(), "bad id;with junk")
			_, err := relay.SendBridgeRequestContext(ctx, "ListProjects", json.RawMessage(`{}`))
			return err
		},
		"SendBridgeRequest": func() error {
			_, err := relay.SendBridgeRequest("ListProjects", json.RawMessage(`{}`))
			return err
		},
	}
	for name, send := range cases {
		t.Run(name, func(t *testing.T) {
			lines := launchedAgainstRaw(t)
			if err := send(); err != nil {
				t.Fatalf("send: %v", err)
			}
			m := wireKeys(t, <-lines)
			if _, ok := m["trace_id"]; ok {
				t.Errorf("trace_id key present on the wire: %v", m)
			}
			if string(m["type"]) != `"ListProjects"` {
				t.Errorf("type = %s, want ListProjects", m["type"])
			}
		})
	}
}
