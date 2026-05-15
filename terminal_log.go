package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// terminalLogger writes a PTY's raw byte stream to two append-only files:
//
//   {dir}/{id}.head.log  — first 64KB, captured once and closed.
//   {dir}/{id}.tail.log  — the rest, rotated when it exceeds tailCapBytes.
//
// Rationale: ANSI streams establish state at the start (cursor home, screen
// clear, SGR resets). Lopping bytes off the front would yield garbled colors
// and a misplaced cursor on replay. Capturing the head preserves the
// program's initial mode-setting; the rolling tail preserves recent output.
// When the tail rotates, a visible marker is written into the new file so a
// reader can see that bytes were dropped.
const (
	headCapBytes      = 64 * 1024
	tailCapBytes      = 1024*1024 - 64*1024 // ~960KB → ≤1MB total per session
	truncationMarker  = "\r\n\x1b[2m[…log truncated…]\x1b[0m\r\n"
)

type terminalLogger struct {
	headPath string
	tailPath string

	mu        sync.Mutex
	headFile  *os.File
	tailFile  *os.File
	headBytes int
	tailBytes int
}

func newTerminalLogger(dir, id string) (*terminalLogger, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("mkdir log dir: %w", err)
	}
	headPath := filepath.Join(dir, id+".head.log")
	tailPath := filepath.Join(dir, id+".tail.log")

	head, err := os.OpenFile(headPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open head log: %w", err)
	}
	tail, err := os.OpenFile(tailPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		_ = head.Close()
		return nil, fmt.Errorf("open tail log: %w", err)
	}

	return &terminalLogger{
		headPath: headPath,
		tailPath: tailPath,
		headFile: head,
		tailFile: tail,
	}, nil
}

// Write tees a chunk of PTY output to the head/tail files. Errors are
// swallowed (logged at debug elsewhere) so a disk hiccup never stalls the
// PTY readLoop.
func (l *terminalLogger) Write(p []byte) {
	if l == nil || len(p) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	// Fill head first. Head and tail transition is contiguous — no marker
	// needed between them on first switchover.
	if l.headFile != nil {
		remaining := headCapBytes - l.headBytes
		if remaining > 0 {
			take := len(p)
			if take > remaining {
				take = remaining
			}
			if _, err := l.headFile.Write(p[:take]); err == nil {
				l.headBytes += take
			}
			p = p[take:]
		}
		if l.headBytes >= headCapBytes {
			_ = l.headFile.Sync()
			_ = l.headFile.Close()
			l.headFile = nil
		}
	}

	if len(p) == 0 || l.tailFile == nil {
		return
	}

	// Rotate the tail when it overflows. The previous window is discarded —
	// we only keep the most recent tailCapBytes. A truncation marker is
	// written into the new file so readers see the gap.
	if l.tailBytes+len(p) > tailCapBytes {
		_ = l.tailFile.Sync()
		_ = l.tailFile.Close()
		f, err := os.OpenFile(l.tailPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			l.tailFile = nil
			return
		}
		l.tailFile = f
		l.tailBytes = 0
		if n, err := l.tailFile.Write([]byte(truncationMarker)); err == nil {
			l.tailBytes += n
		}
	}

	if n, err := l.tailFile.Write(p); err == nil {
		l.tailBytes += n
	}
}

// Close flushes and closes both files. The files remain on disk for replay.
func (l *terminalLogger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.headFile != nil {
		_ = l.headFile.Sync()
		_ = l.headFile.Close()
		l.headFile = nil
	}
	if l.tailFile != nil {
		_ = l.tailFile.Sync()
		_ = l.tailFile.Close()
		l.tailFile = nil
	}
}

// errTerminalLogNotFound is returned when neither log file exists for the
// requested terminal ID. The HTTP handler maps this to 404; other errors
// (validation, disk I/O) become 500 / 400.
var errTerminalLogNotFound = errors.New("terminal log not found")

// readTerminalLog buffers the stitched head+tail into a byte slice. Used
// only by tests — production callers stream via openTerminalLogReaders +
// io.Copy to avoid loading the whole 1 MB cap into memory.
func readTerminalLog(dir, id string) ([]byte, error) {
	head, tail, err := openTerminalLogReaders(dir, id)
	if err != nil {
		return nil, err
	}
	var out []byte
	for _, f := range []*os.File{head, tail} {
		if f == nil {
			continue
		}
		b, rerr := os.ReadFile(f.Name())
		_ = f.Close()
		if rerr != nil {
			return nil, rerr
		}
		out = append(out, b...)
	}
	return out, nil
}

// openTerminalLogReaders returns open file handles for the head and tail
// log files of a terminal. Either may be nil if that file doesn't exist.
// Returns errTerminalLogNotFound if neither exists. The caller owns
// Close() on the returned handles.
func openTerminalLogReaders(dir, id string) (head, tail *os.File, err error) {
	if !isValidTerminalID(id) {
		return nil, nil, fmt.Errorf("invalid terminal id")
	}
	head, headErr := os.Open(filepath.Join(dir, id+".head.log"))
	if headErr != nil && !os.IsNotExist(headErr) {
		return nil, nil, headErr
	}
	tail, tailErr := os.Open(filepath.Join(dir, id+".tail.log"))
	if tailErr != nil && !os.IsNotExist(tailErr) {
		if head != nil {
			_ = head.Close()
		}
		return nil, nil, tailErr
	}
	if head == nil && tail == nil {
		return nil, nil, errTerminalLogNotFound
	}
	return head, tail, nil
}

// sweepTerminalLogs deletes per-session log files older than maxAge. After
// that pass, if the directory's total size still exceeds maxTotalBytes,
// removes the oldest remaining files until the cap is satisfied. Returns
// the number of files removed; errors on individual files are logged at
// debug and don't abort the sweep.
//
// Time-based eviction is the primary policy (30 days is plenty for
// "I want to see what that scheduled task did last week"); the byte cap is
// a safety net for hosts that churn through many short sessions.
func sweepTerminalLogs(dir string, maxAge time.Duration, maxTotalBytes int64) (removed int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	type fileEntry struct {
		path  string
		size  int64
		mtime time.Time
	}
	keep := make([]fileEntry, 0, len(entries))
	cutoff := time.Now().Add(-maxAge)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Only touch our own files. A user dropping unrelated files in this
		// dir is unusual but we still want to fail safely.
		name := e.Name()
		if !strings.HasSuffix(name, ".head.log") && !strings.HasSuffix(name, ".tail.log") {
			continue
		}
		full := filepath.Join(dir, name)
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(full); err == nil {
				removed++
			}
			continue
		}
		keep = append(keep, fileEntry{path: full, size: info.Size(), mtime: info.ModTime()})
	}

	if maxTotalBytes <= 0 {
		return removed, nil
	}

	var total int64
	for _, f := range keep {
		total += f.size
	}
	if total <= maxTotalBytes {
		return removed, nil
	}

	// Sort oldest first and trim until under the cap.
	sort.Slice(keep, func(i, j int) bool { return keep[i].mtime.Before(keep[j].mtime) })
	for _, f := range keep {
		if total <= maxTotalBytes {
			break
		}
		if err := os.Remove(f.path); err == nil {
			removed++
			total -= f.size
		}
	}
	return removed, nil
}

// isValidTerminalID validates a UUID v4 string shape. Protects against path
// traversal in readTerminalLog when serving via HTTP. Terminal IDs are minted
// by uuid.New() in TerminalManager.Create so the shape is fixed.
func isValidTerminalID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, r := range id {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}
