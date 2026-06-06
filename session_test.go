package main

// SessionManager + SessionStore lifecycle coverage. These tests drive the
// manager directly (no HTTP/WS) so a failure points at the manager itself,
// not at routing.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// newTestSessionManager returns a SessionManager wired to a temp-dir store
// with a per-session FakeProvider factory. Each CreateSession call produces
// a fresh provider so killing one doesn't cascade across sessions.
func newTestSessionManager(t *testing.T) *SessionManager {
	t.Helper()
	dir := t.TempDir()
	store := NewSessionStore(filepath.Join(dir, "sessions"))
	perms := NewPermissionManager()
	mgr := NewSessionManager(store, perms)
	mgr.SetDataDir(dir)
	mgr.SetProviderFactory(func(_ *Session, h EventHandler) (Provider, error) {
		return NewFakeProvider(h), nil
	})
	t.Cleanup(mgr.StopAll)
	return mgr
}

func mustCreateSession(t *testing.T, mgr *SessionManager, name string) *Session {
	t.Helper()
	sess, err := mgr.CreateSession("", t.TempDir(), name, "fake/m1", "", false, "fake", nil)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return sess
}

// ---------------------------------------------------------------------------
// Persistence + lazy reload
// ---------------------------------------------------------------------------

func TestSession_PersistsToDiskOnEnd(t *testing.T) {
	mgr := newTestSessionManager(t)
	sess := mustCreateSession(t, mgr, "to-persist")

	// Inject one message so we can prove it survives the round-trip.
	sess.mu.Lock()
	sess.Messages = []Message{{Role: "user", Content: json.RawMessage(`"hello"`)}}
	sess.mu.Unlock()

	mgr.EndSession(sess.ID)

	// File must exist on disk under sessions/{id}.json.
	path := filepath.Join(mgr.dataDir, "sessions", sess.ID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected persisted file at %s: %v", path, err)
	}
	data, _ := os.ReadFile(path)
	var disk Session
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatalf("decode persisted: %v", err)
	}
	if len(disk.Messages) != 1 {
		t.Errorf("Messages on disk: got %d, want 1 (file=%s)", len(disk.Messages), string(data))
	}
}

func TestSession_GetSession_LazyLoadsFromDisk(t *testing.T) {
	mgr := newTestSessionManager(t)
	sess := mustCreateSession(t, mgr, "to-reload")
	id := sess.ID
	mgr.EndSession(id)

	// EndSession dropped it from memory. A subsequent GetSession must
	// transparently rehydrate from disk.
	rehydrated, ok := mgr.GetSession(id)
	if !ok {
		t.Fatal("GetSession returned false after EndSession; expected lazy reload")
	}
	if rehydrated.ID != id {
		t.Errorf("rehydrated.ID=%q, want %q", rehydrated.ID, id)
	}

	// Second GetSession should hit the in-memory cache.
	again, ok := mgr.GetSession(id)
	if !ok || again != rehydrated {
		t.Errorf("expected cache hit on second GetSession; got ok=%v same=%v", ok, again == rehydrated)
	}
}

func TestSession_DeleteSession_RemovesFromMemoryAndDisk(t *testing.T) {
	mgr := newTestSessionManager(t)
	sess := mustCreateSession(t, mgr, "to-delete")
	id := sess.ID
	mgr.EndSession(id) // ensure on disk first

	mgr.DeleteSession(id)

	path := filepath.Join(mgr.dataDir, "sessions", id+".json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected file gone, got err=%v", err)
	}
	// GetSession must also fail — neither memory nor disk has it.
	if _, ok := mgr.GetSession(id); ok {
		t.Error("GetSession returned a session after DeleteSession")
	}
}

func TestSession_ClearSession_WipesHistory_KeepsSession(t *testing.T) {
	mgr := newTestSessionManager(t)
	sess := mustCreateSession(t, mgr, "to-clear")
	sess.mu.Lock()
	sess.Messages = []Message{
		{Role: "user", Content: json.RawMessage(`"a"`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"b"}]`)},
	}
	sess.mu.Unlock()

	if err := mgr.ClearSession(sess.ID); err != nil {
		t.Fatalf("ClearSession: %v", err)
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if len(sess.Messages) != 0 {
		t.Errorf("Messages: got %d, want 0", len(sess.Messages))
	}
	if sess.Model != "fake/m1" {
		t.Errorf("Model changed after clear: got %q", sess.Model)
	}
}

func TestSession_RenameSession_UpdatesNameInMemoryAndDisk(t *testing.T) {
	mgr := newTestSessionManager(t)
	sess := mustCreateSession(t, mgr, "old-name")
	if err := mgr.RenameSession(sess.ID, "new-name"); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	if sess.Name != "new-name" {
		t.Errorf("in-memory name: got %q", sess.Name)
	}
	// Disk should reflect the rename too — RenameSession saves.
	path := filepath.Join(mgr.dataDir, "sessions", sess.ID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read disk: %v", err)
	}
	var disk Session
	_ = json.Unmarshal(data, &disk)
	if disk.Name != "new-name" {
		t.Errorf("disk name: got %q", disk.Name)
	}
}

// ---------------------------------------------------------------------------
// Listing semantics
// ---------------------------------------------------------------------------

func TestSession_ListSessions_ExcludesHeadless(t *testing.T) {
	mgr := newTestSessionManager(t)

	visible := mustCreateSession(t, mgr, "visible")
	headless := mustCreateSession(t, mgr, "headless")
	headless.mu.Lock()
	headless.Headless = true
	headless.mu.Unlock()

	list := mgr.ListSessions()
	foundVisible, foundHeadless := false, false
	for _, e := range list {
		if e["id"] == visible.ID {
			foundVisible = true
		}
		if e["id"] == headless.ID {
			foundHeadless = true
		}
	}
	if !foundVisible {
		t.Errorf("visible session missing from list")
	}
	if foundHeadless {
		t.Errorf("headless session leaked into list")
	}
}

func TestSession_ListSessions_MergesInMemoryAndDisk(t *testing.T) {
	mgr := newTestSessionManager(t)

	inMem := mustCreateSession(t, mgr, "live")
	onDisk := mustCreateSession(t, mgr, "persisted")
	mgr.EndSession(onDisk.ID) // remove from memory; remains on disk

	list := mgr.ListSessions()
	foundLive, foundPersisted := false, false
	for _, e := range list {
		if e["id"] == inMem.ID {
			foundLive = true
			if e["active"] != true {
				t.Errorf("live session active flag: got %v, want true", e["active"])
			}
		}
		if e["id"] == onDisk.ID {
			foundPersisted = true
			if e["active"] != false {
				t.Errorf("persisted session active flag: got %v, want false", e["active"])
			}
		}
	}
	if !foundLive || !foundPersisted {
		t.Errorf("merge missing entries: live=%v persisted=%v list=%v", foundLive, foundPersisted, list)
	}
}

// ---------------------------------------------------------------------------
// Pi-only routes reject non-pi sessions
// ---------------------------------------------------------------------------

func TestSession_SetPiModel_RejectsNonPiProvider(t *testing.T) {
	mgr := newTestSessionManager(t)
	sess := mustCreateSession(t, mgr, "not-pi")
	if err := mgr.SetPiModel(sess.ID, "anthropic", "claude-sonnet-4"); err == nil {
		t.Error("expected error setting pi model on non-pi session, got nil")
	}
}

func TestSession_SetPiThinkingLevel_RejectsNonPiProvider(t *testing.T) {
	mgr := newTestSessionManager(t)
	sess := mustCreateSession(t, mgr, "not-pi")
	if err := mgr.SetPiThinkingLevel(sess.ID, "high"); err == nil {
		t.Error("expected error setting thinking level on non-pi session, got nil")
	}
}

// ---------------------------------------------------------------------------
// Headless sweeper
// ---------------------------------------------------------------------------

func TestSession_SweepSessions_DeletesOldHeadlessOnly(t *testing.T) {
	dir := t.TempDir()
	sessionsDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Three files: one fresh headless, one old headless, one old non-headless.
	writeSession := func(id string, headless bool, mtimeOffset time.Duration) {
		path := filepath.Join(sessionsDir, id+".json")
		body, _ := json.Marshal(map[string]any{
			"sessionId": id,
			"headless":  headless,
		})
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
		if mtimeOffset != 0 {
			past := time.Now().Add(mtimeOffset)
			if err := os.Chtimes(path, past, past); err != nil {
				t.Fatalf("chtimes %s: %v", id, err)
			}
		}
	}
	writeSession("fresh-headless", true, 0)
	writeSession("old-headless", true, -10*24*time.Hour)
	writeSession("old-keeper", false, -10*24*time.Hour)

	removed, _, err := sweepSessions(sessionsDir, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("sweepSessions: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed: got %d, want 1", removed)
	}

	// old-headless should be gone; the other two should remain.
	if _, err := os.Stat(filepath.Join(sessionsDir, "old-headless.json")); !os.IsNotExist(err) {
		t.Errorf("old-headless not deleted: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(sessionsDir, "fresh-headless.json")); err != nil {
		t.Errorf("fresh-headless wrongly deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessionsDir, "old-keeper.json")); err != nil {
		t.Errorf("old-keeper (non-headless) wrongly deleted: %v", err)
	}
}

func TestSession_SweepSessions_NoSessionsDir_IsNoOp(t *testing.T) {
	dir := t.TempDir()
	removed, livePi, err := sweepSessions(filepath.Join(dir, "nonexistent"), time.Hour)
	if err != nil {
		t.Fatalf("sweep on missing dir: %v", err)
	}
	if removed != 0 || len(livePi) != 0 {
		t.Errorf("expected zero work on missing dir; got removed=%d livePi=%d", removed, len(livePi))
	}
}

func TestSession_SweepSessions_HarvestsLivePiIDs(t *testing.T) {
	dir := t.TempDir()
	sessionsDir := filepath.Join(dir, "sessions")
	os.MkdirAll(sessionsDir, 0o700)

	// One surviving session references a piSessionId; the sweeper should
	// return it in livePi so pi-session GC knows to skip it.
	body, _ := json.Marshal(map[string]any{
		"sessionId":     "s1",
		"providerState": map[string]string{"piSessionId": "pi-abc-123"},
	})
	os.WriteFile(filepath.Join(sessionsDir, "s1.json"), body, 0o600)

	_, livePi, err := sweepSessions(sessionsDir, time.Hour)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, ok := livePi["pi-abc-123"]; !ok {
		t.Errorf("expected pi-abc-123 in livePi; got %v", livePi)
	}
}

// ---------------------------------------------------------------------------
// Project policy enforcement (the production code path the WS+API tests
// only smoke-test via the api permission endpoint)
// ---------------------------------------------------------------------------

func TestPolicy_MatchToolRule_BareName(t *testing.T) {
	if !MatchToolRule("Read", `{}`, []string{"Read"}) {
		t.Error("bare-name pattern should match same-named tool")
	}
	if MatchToolRule("Write", `{}`, []string{"Read"}) {
		t.Error("bare-name pattern should not match a different tool")
	}
}

func TestPolicy_MatchToolRule_ArgPrefix(t *testing.T) {
	// "Bash:ls" should match any Bash invocation whose serialized toolInput
	// (after stripping the leading "{") contains "ls". This is the pattern
	// users put in project policy to allowlist specific subcommands.
	cases := []struct {
		toolName  string
		toolInput string
		pattern   string
		want      bool
		desc      string
	}{
		{"Bash", `{"command":"ls -la"}`, "Bash:ls", true, "matching prefix matches"},
		{"Bash", `{"command":"rm -rf /"}`, "Bash:ls", false, "non-matching prefix does not match"},
		{"Read", `{"path":"/etc/passwd"}`, "Bash:ls", false, "different tool ignored"},
		{"Bash", `{ "command":"ls"}`, "Bash:ls", true, "leading whitespace tolerated"},
	}
	for _, tc := range cases {
		got := MatchToolRule(tc.toolName, tc.toolInput, []string{tc.pattern})
		if got != tc.want {
			t.Errorf("%s: MatchToolRule(%q, %q, [%q]) = %v, want %v",
				tc.desc, tc.toolName, tc.toolInput, tc.pattern, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Concurrent GetSession (lazy load race)
// ---------------------------------------------------------------------------

func TestSession_GetSession_ConcurrentLazyLoad_ReturnsSameInstance(t *testing.T) {
	mgr := newTestSessionManager(t)
	sess := mustCreateSession(t, mgr, "race")
	id := sess.ID
	mgr.EndSession(id) // drop from memory, leave on disk

	const N = 16
	var wg sync.WaitGroup
	results := make([]*Session, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			s, ok := mgr.GetSession(id)
			if !ok {
				t.Errorf("goroutine %d: GetSession returned false", i)
				return
			}
			results[i] = s
		}(i)
	}
	wg.Wait()

	// All goroutines must converge on the same pointer — that's the whole
	// point of the double-check in GetSession.
	first := results[0]
	for i := 1; i < N; i++ {
		if results[i] != first {
			t.Errorf("goroutine %d: returned different pointer (rehydration race produced duplicates)", i)
			break
		}
	}
}

// ---------------------------------------------------------------------------
// CreateSession edge cases
// ---------------------------------------------------------------------------

func TestSession_CreateSession_GeneratesUniqueIDs(t *testing.T) {
	mgr := newTestSessionManager(t)
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		s := mustCreateSession(t, mgr, "s"+strconv.Itoa(i))
		if seen[s.ID] {
			t.Fatalf("duplicate sessionID %q at iter %d", s.ID, i)
		}
		seen[s.ID] = true
	}
}
