package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTerminalLogger_HeadAndTail(t *testing.T) {
	dir := t.TempDir()
	id := "11111111-2222-3333-4444-555555555555"

	lg, err := newTerminalLogger(dir, id)
	if err != nil {
		t.Fatalf("newTerminalLogger: %v", err)
	}

	// Two writes that together overflow the head into the tail.
	first := bytes.Repeat([]byte{'A'}, headCapBytes-10)
	second := bytes.Repeat([]byte{'B'}, 100) // 10 spill into head, 90 into tail
	lg.Write(first)
	lg.Write(second)
	lg.Close()

	got, err := readTerminalLog(dir, id)
	if err != nil {
		t.Fatalf("readTerminalLog: %v", err)
	}
	if len(got) != len(first)+len(second) {
		t.Fatalf("stitched length = %d, want %d", len(got), len(first)+len(second))
	}
	// Bytes must be in the original order — head transitions to tail
	// contiguously with no truncation marker between them on first switchover.
	if !bytes.Equal(got[:len(first)], first) || !bytes.Equal(got[len(first):], second) {
		t.Fatal("stitched bytes don't match input order")
	}
	if strings.Contains(string(got), "log truncated") {
		t.Fatal("unexpected truncation marker on first head→tail transition")
	}
}

func TestTerminalLogger_TailRotation(t *testing.T) {
	dir := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	lg, err := newTerminalLogger(dir, id)
	if err != nil {
		t.Fatalf("newTerminalLogger: %v", err)
	}

	// Fill head exactly, then write enough to the tail to force one rotation.
	lg.Write(bytes.Repeat([]byte{'H'}, headCapBytes))
	lg.Write(bytes.Repeat([]byte{'X'}, tailCapBytes))            // fills tail
	lg.Write(bytes.Repeat([]byte{'Y'}, 1024))                    // triggers rotation
	lg.Close()

	got, err := readTerminalLog(dir, id)
	if err != nil {
		t.Fatalf("readTerminalLog: %v", err)
	}
	// After rotation, head is preserved, the X's are dropped, and we expect
	// the truncation marker followed by the Y's.
	if !bytes.HasPrefix(got, bytes.Repeat([]byte{'H'}, headCapBytes)) {
		t.Fatal("head bytes missing or wrong")
	}
	if !strings.Contains(string(got), "log truncated") {
		t.Fatal("expected truncation marker after rotation")
	}
	if !bytes.HasSuffix(got, bytes.Repeat([]byte{'Y'}, 1024)) {
		t.Fatal("post-rotation bytes missing")
	}
	// Total size cap: head + marker + tail ≤ 1MB + a small slack.
	if len(got) > 1024*1024+len(truncationMarker)+1024 {
		t.Fatalf("stitched size %d exceeds expected cap", len(got))
	}
}

func TestTerminalLogger_NilSafe(t *testing.T) {
	var lg *terminalLogger
	lg.Write([]byte("anything"))
	lg.Close()
}

func TestReadTerminalLog_RejectsBadID(t *testing.T) {
	cases := []string{
		"",
		"../etc/passwd",
		"not-a-uuid",
		"11111111-2222-3333-4444-55555555555", // 35 chars
		"11111111x2222-3333-4444-555555555555", // wrong separator
		"11111111-2222-3333-4444-5555555555gg", // non-hex
	}
	for _, id := range cases {
		if isValidTerminalID(id) {
			t.Errorf("isValidTerminalID(%q) = true, want false", id)
		}
	}
	if !isValidTerminalID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee") {
		t.Error("isValidTerminalID rejected a well-formed UUID")
	}
}

func TestReadTerminalLog_NoFiles(t *testing.T) {
	dir := t.TempDir()
	_, err := readTerminalLog(dir, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if err == nil {
		t.Fatal("expected error when no log files exist")
	}
}

func TestSweepTerminalLogs_RemovesOldFiles(t *testing.T) {
	dir := t.TempDir()
	makeLog := func(name string, age time.Duration, size int) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}

	// Three sessions: old, recent-small, recent-big. Sweep with 7-day age
	// limit and a 1KB byte cap should remove the old one (age) and then
	// the recent-big one (bytes).
	makeLog("aaaaaaaa-bbbb-cccc-dddd-111111111111.head.log", 10*24*time.Hour, 100)
	makeLog("aaaaaaaa-bbbb-cccc-dddd-111111111111.tail.log", 10*24*time.Hour, 200)
	makeLog("bbbbbbbb-bbbb-cccc-dddd-222222222222.head.log", 1*time.Hour, 100)
	makeLog("bbbbbbbb-bbbb-cccc-dddd-222222222222.tail.log", 1*time.Hour, 200)
	makeLog("cccccccc-bbbb-cccc-dddd-333333333333.tail.log", 30*time.Minute, 2048)

	removed, err := sweepTerminalLogs(dir, 7*24*time.Hour, 1024)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed < 2 {
		t.Fatalf("expected at least 2 files removed, got %d", removed)
	}

	// The old session's files should be gone.
	if _, err := os.Stat(filepath.Join(dir, "aaaaaaaa-bbbb-cccc-dddd-111111111111.head.log")); !os.IsNotExist(err) {
		t.Fatal("old head.log should have been removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "aaaaaaaa-bbbb-cccc-dddd-111111111111.tail.log")); !os.IsNotExist(err) {
		t.Fatal("old tail.log should have been removed")
	}
}

func TestSweepTerminalLogs_IgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	// Drop a file that doesn't match our naming convention — the sweeper
	// must leave it alone.
	stranger := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(stranger, []byte("hi"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	mt := time.Now().Add(-365 * 24 * time.Hour)
	_ = os.Chtimes(stranger, mt, mt)

	if _, err := sweepTerminalLogs(dir, 7*24*time.Hour, 0); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(stranger); err != nil {
		t.Fatalf("non-log file was removed: %v", err)
	}
}
