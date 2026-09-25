package registry

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"relayllm/internal/config"
	"relayllm/internal/testutil"
)

// oneShotPerConnServer answers the first request on every connection with a
// keep-alive /models response, then reads any later request on that same
// connection and never answers it: an upstream that vanished without a FIN
// or RST, as seen from a pooled connection. A fresh connection is always
// served. When tlsCfg is non-nil each connection is wrapped in TLS.
func oneShotPerConnServer(t *testing.T, tlsCfg *tls.Config) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		close(done)
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})

	body := `{"data":[{"id":"m1"}]}`
	reply := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: keep-alive\r\n\r\n%s", len(body), body)

	serve := func(c net.Conn) {
		if tlsCfg != nil {
			c = tls.Server(c, tlsCfg)
		}
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		io.Copy(io.Discard, req.Body)
		if _, err := io.WriteString(c, reply); err != nil {
			return
		}
		if _, err := http.ReadRequest(br); err != nil {
			return
		}
		<-done
	}

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go serve(c)
		}
	}()
	return ln.Addr().String()
}

func TestFetchOpenAIModels_NeverReusesAProbeConnection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		endpoint func(t *testing.T) config.OpenAIEndpoint
	}{
		{"plain http", func(t *testing.T) config.OpenAIEndpoint {
			return config.OpenAIEndpoint{Name: "p1", BaseURL: "http://" + oneShotPerConnServer(t, nil)}
		}},
		{"https with caFile", func(t *testing.T) config.OpenAIEndpoint {
			ca := testutil.NewTestCA(t)
			leaf := ca.IssueLeaf(t, 2)
			addr := oneShotPerConnServer(t, &tls.Config{Certificates: []tls.Certificate{leaf.TLSCert}})
			return config.OpenAIEndpoint{Name: "p1", BaseURL: "https://" + addr, CAFile: ca.WriteCAFile(t)}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ep := tc.endpoint(t)
			if err := config.PrepareEndpointTransports(&ep, false); err != nil {
				t.Fatalf("PrepareEndpointTransports: %v", err)
			}

			for i := 1; i <= 2; i++ {
				start := time.Now()
				models, err := FetchOpenAIModels(context.Background(), ep)
				elapsed := time.Since(start)
				if err != nil {
					t.Fatalf("probe %d: FetchOpenAIModels after %v: %v", i, elapsed, err)
				}
				if len(models) != 1 || models[0].ID != "m1" {
					t.Fatalf("probe %d: models = %+v, want one model m1", i, models)
				}
				if elapsed >= 2*time.Second {
					t.Fatalf("probe %d took %v, want under 2s", i, elapsed)
				}
			}
		})
	}
}
