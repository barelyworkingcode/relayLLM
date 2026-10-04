package registry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"relayllm/internal/config"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// An unreachable endpoint whose baseURL carries a userinfo token and a query
// credential must not leak either one into the logs or into the stored
// status error that the status API serves.
func TestProxyRegistry_UnreachableEndpoint_DoesNotLeakURLCredentials(t *testing.T) {
	const userToken = "sk-USERTOKEN"
	const queryKey = "QSECRET"

	var logs lockedBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	reg := NewProxyRegistry(&config.OpenAIConfig{
		Endpoints: []config.OpenAIEndpoint{{
			Name:    "acme",
			BaseURL: "http://" + userToken + "@127.0.0.1:1/v1?api_key=" + queryKey,
		}},
	})

	snap := reg.Snapshot(context.Background())
	if len(snap) != 1 {
		t.Fatalf("snapshot entries = %d, want 1", len(snap))
	}
	if snap[0].Online {
		t.Fatal("endpoint on port 1 reported online; test setup invalid")
	}
	if snap[0].Err == "" {
		t.Fatal("offline status has empty Err; test would pass vacuously")
	}
	if logs.String() == "" {
		t.Fatal("no log output captured; test would pass vacuously")
	}

	for _, secret := range []string{userToken, queryKey} {
		if strings.Contains(snap[0].Err, secret) {
			t.Errorf("status Err leaks %q: %s", secret, snap[0].Err)
		}
		if strings.Contains(logs.String(), secret) {
			t.Errorf("log output leaks %q: %s", secret, logs.String())
		}
	}
}
