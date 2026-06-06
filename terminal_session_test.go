package main

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

// TestTerminalSession_PTYExitAndLog spawns a real shell with extraArgs,
// asserts the captured exit code, scrollback contents, and on-disk log
// persistence. Verifies the steps 1+2 plumbing in one go.
func TestTerminalSession_PTYExitAndLog(t *testing.T) {
	tmpDir := t.TempDir()
	logDir := filepath.Join(tmpDir, "terminal_logs")

	store := NewTemplateStore(tmpDir)
	if err := store.Load(map[string]TerminalTemplate{
		"test-sh": {Name: "test-sh", Command: "/bin/sh"},
	}); err != nil {
		t.Fatalf("template load: %v", err)
	}

	mgr := NewTerminalManager(store, logDir)
	exitCh := make(chan int, 1)
	mgr.SetExitHandler(func(_ string, code int) { exitCh <- code })

	sess, err := mgr.Create("test-sh", "test", tmpDir, "", 80, 24, []string{"-c", "echo hi-from-pty; exit 7"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	select {
	case code := <-exitCh:
		if code != 7 {
			t.Fatalf("exit code = %d, want 7", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for exit")
	}

	// Poll for the log file — readLoop's defer flush races with onExit.
	deadline := time.Now().Add(2 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		data, _ = readTerminalLog(logDir, sess.ID)
		if bytes.Contains(data, []byte("hi-from-pty")) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !bytes.Contains(data, []byte("hi-from-pty")) {
		t.Fatalf("log file missing expected output: %q", data)
	}

	// Scrollback (in-memory ring) should mirror the disk content.
	if !bytes.Contains(sess.ScrollbackBytes(), []byte("hi-from-pty")) {
		t.Fatal("scrollback missing expected output")
	}
}
