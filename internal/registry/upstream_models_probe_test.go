//go:build unix

package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"syscall"
	"testing"
	"time"

	"relayllm/internal/config"
)

// blackHoleAddr returns a loopback address whose TCP connects never complete.
// A listener with a full accept queue drops incoming SYNs silently on both
// macOS and Linux, which reproduces a host that is up but whose port drops
// packets, without depending on any real network.
func blackHoleAddr(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatalf("listen: %v", err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatalf("getsockname: %v", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", sa.(*syscall.SockaddrInet4).Port)

	for i := 0; i < 4096; i++ {
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return addr
		}
		if err != nil {
			t.Fatalf("filling accept queue: %v", err)
		}
		t.Cleanup(func() { c.Close() })
	}
	t.Skip("accept queue never filled; cannot black-hole a loopback port here")
	return ""
}

// silentTLSAddr returns a loopback address that completes the TCP handshake
// but never answers the TLS ClientHello.
func silentTLSAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var held []net.Conn
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	return ln.Addr().String()
}

func TestFetchOpenAIModels_ConnectNeverCompletes_FailsFast(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		baseURL func(t *testing.T) string
		prepare bool
	}{
		{"tcp black hole, prepared endpoint", func(t *testing.T) string { return "http://" + blackHoleAddr(t) }, true},
		{"tcp black hole, unprepared endpoint", func(t *testing.T) string { return "http://" + blackHoleAddr(t) }, false},
		{"tls handshake stalls", func(t *testing.T) string { return "https://" + silentTLSAddr(t) }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ep := config.OpenAIEndpoint{Name: "p1", BaseURL: tc.baseURL(t)}
			if tc.prepare {
				if err := config.PrepareEndpointTransports(&ep, false); err != nil {
					t.Fatalf("PrepareEndpointTransports: %v", err)
				}
			}

			start := time.Now()
			_, err := FetchOpenAIModels(context.Background(), ep)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatal("expected an error from an endpoint that never connects")
			}
			if elapsed >= 2*time.Second {
				t.Errorf("FetchOpenAIModels took %v, want under 2s (err: %v)", elapsed, err)
			}
		})
	}
}

func TestFetchOpenAIModels_SlowButReachable_Succeeds(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "m1"}}})
	}))
	t.Cleanup(srv.Close)

	ep := config.OpenAIEndpoint{Name: "p1", BaseURL: srv.URL}
	if err := config.PrepareEndpointTransports(&ep, false); err != nil {
		t.Fatalf("PrepareEndpointTransports: %v", err)
	}

	models, err := FetchOpenAIModels(context.Background(), ep)
	if err != nil {
		t.Fatalf("FetchOpenAIModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "m1" {
		t.Errorf("models = %+v, want one model m1", models)
	}
}
