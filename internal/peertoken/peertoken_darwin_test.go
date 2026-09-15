//go:build darwin

package peertoken

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestFromConn_ReportsThisProcessOnBothEnds(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	defer server.Close()

	fromServer, err := FromConn(server)
	if err != nil {
		t.Fatalf("server side: %v", err)
	}
	fromClient, err := FromConn(client)
	if err != nil {
		t.Fatalf("client side: %v", err)
	}
	if got, want := int(fromServer.PID()), os.Getpid(); got != want {
		t.Fatalf("pid = %d, want %d", got, want)
	}
	if fromServer.PIDVersion() == 0 {
		t.Fatal("pidversion is zero; the byte offset is wrong")
	}
	if fromServer.Process() != fromClient.Process() {
		t.Fatalf("both ends are this process but disagree: %+v vs %+v", fromServer.Process(), fromClient.Process())
	}
}

func TestFromConn_RefusesTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, _ := ln.Accept()
		if c != nil {
			_ = c.Close()
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tok, err := FromConn(c)
	if err == nil || tok.Valid() {
		t.Fatalf("TCP conn produced a token: %+v, err=%v", tok.Process(), err)
	}
}

func TestForProcessForTest_RoundTrips(t *testing.T) {
	tok := ForProcessForTest(4242, 7)
	if tok.PID() != 4242 || tok.PIDVersion() != 7 || !tok.Valid() {
		t.Fatalf("round trip: %+v", tok.Process())
	}
	if (Token{}).Valid() {
		t.Fatal("the zero token must not be valid")
	}
}
