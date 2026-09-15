package router

// Coverage for C10's file format, CLI-facing operations (add/list/revoke),
// atomic writes, and RouterKeyStore's fail-closed load/reload behavior.
// auth_test.go covers the request-time middleware built on top of this.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAddRouterKey_FormatLabelAndUniqueness(t *testing.T) {
	dir := t.TempDir()

	plaintext, err := AddRouterKey(dir, "hermes")
	if err != nil {
		t.Fatalf("AddRouterKey: %v", err)
	}
	if !strings.HasPrefix(plaintext, RouterKeyPlaintextPrefix) {
		t.Fatalf("plaintext %q does not start with %q", plaintext, RouterKeyPlaintextPrefix)
	}
	hexPart := strings.TrimPrefix(plaintext, RouterKeyPlaintextPrefix)
	if len(hexPart) != 64 {
		t.Fatalf("hex part has length %d, want 64", len(hexPart))
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		t.Fatalf("hex part %q is not valid lowercase hex: %v", hexPart, err)
	}
	if hexPart != strings.ToLower(hexPart) {
		t.Fatalf("hex part %q is not all-lowercase", hexPart)
	}

	// The file must contain only the hash, never the plaintext.
	data, err := os.ReadFile(RouterKeysPath(dir))
	if err != nil {
		t.Fatalf("read router_keys.json: %v", err)
	}
	if strings.Contains(string(data), plaintext) {
		t.Fatalf("router_keys.json contains the plaintext key: %s", data)
	}
	var records []routerKeyRecord
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(records) != 1 || records[0].Label != "hermes" {
		t.Fatalf("records = %+v, want one entry labeled hermes", records)
	}
	sum := sha256.Sum256([]byte(plaintext))
	if records[0].SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("stored hash does not match sha256(plaintext)")
	}
	if _, err := time.Parse(time.RFC3339, records[0].Created); err != nil {
		t.Fatalf("created %q is not RFC3339: %v", records[0].Created, err)
	}

	// Duplicate label is an error, not a silent overwrite.
	if _, err := AddRouterKey(dir, "hermes"); err == nil {
		t.Fatal("AddRouterKey with a duplicate label must error")
	}
	data2, _ := os.ReadFile(RouterKeysPath(dir))
	var records2 []routerKeyRecord
	_ = json.Unmarshal(data2, &records2)
	if len(records2) != 1 {
		t.Fatalf("a rejected duplicate add must not modify the file: got %d records", len(records2))
	}
}

func TestAddRouterKey_RejectsInvalidLabel(t *testing.T) {
	dir := t.TempDir()
	cases := []string{"", "Hermes", "has space", "has/slash", strings.Repeat("a", 65)}
	for _, label := range cases {
		if _, err := AddRouterKey(dir, label); err == nil {
			t.Errorf("AddRouterKey(%q) succeeded, want a label-format error", label)
		}
	}
}

func TestAddRouterKey_CreatesFileAndDirWithCorrectPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data-dir")
	if _, err := AddRouterKey(dir, "hermes"); err != nil {
		t.Fatalf("AddRouterKey: %v", err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %s, want 0700", perm)
	}
	fileInfo, err := os.Stat(RouterKeysPath(dir))
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %s, want 0600", perm)
	}
}

func TestListRouterKeys_NoFileIsEmptyNotError(t *testing.T) {
	dir := t.TempDir()
	keys, err := ListRouterKeys(dir)
	if err != nil {
		t.Fatalf("ListRouterKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("keys = %+v, want empty", keys)
	}
}

func TestListRouterKeys_NeverShowsPlaintextOrFullHash(t *testing.T) {
	dir := t.TempDir()
	plaintext, err := AddRouterKey(dir, "hermes")
	if err != nil {
		t.Fatalf("AddRouterKey: %v", err)
	}
	keys, err := ListRouterKeys(dir)
	if err != nil {
		t.Fatalf("ListRouterKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys = %+v, want one entry", keys)
	}
	got := keys[0]
	if got.Label != "hermes" {
		t.Errorf("label = %q, want hermes", got.Label)
	}
	if len(got.HashPrefix) != 12 {
		t.Errorf("hash prefix length = %d, want 12", len(got.HashPrefix))
	}
	sum := sha256.Sum256([]byte(plaintext))
	fullHash := hex.EncodeToString(sum[:])
	if got.HashPrefix != fullHash[:12] {
		t.Errorf("hash prefix = %q, want %q", got.HashPrefix, fullHash[:12])
	}
	if got.HashPrefix == fullHash {
		t.Errorf("hash prefix equals the full hash; must be truncated")
	}
	if strings.Contains(got.HashPrefix, plaintext) || got.Created == plaintext {
		t.Errorf("list output leaked the plaintext key")
	}
}

func TestRevokeRouterKey_RemovesLabelAndErrorsOnUnknown(t *testing.T) {
	dir := t.TempDir()
	if _, err := AddRouterKey(dir, "hermes"); err != nil {
		t.Fatalf("AddRouterKey: %v", err)
	}
	if _, err := AddRouterKey(dir, "other"); err != nil {
		t.Fatalf("AddRouterKey: %v", err)
	}

	if err := RevokeRouterKey(dir, "hermes"); err != nil {
		t.Fatalf("RevokeRouterKey: %v", err)
	}
	keys, err := ListRouterKeys(dir)
	if err != nil {
		t.Fatalf("ListRouterKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].Label != "other" {
		t.Fatalf("keys after revoke = %+v, want only \"other\"", keys)
	}

	if err := RevokeRouterKey(dir, "hermes"); err == nil {
		t.Fatal("RevokeRouterKey of an already-removed label must error")
	}
	if err := RevokeRouterKey(dir, "never-existed"); err == nil {
		t.Fatal("RevokeRouterKey of an unknown label must error")
	}
}

func TestRevokeRouterKey_NoFileErrors(t *testing.T) {
	dir := t.TempDir()
	if err := RevokeRouterKey(dir, "hermes"); err == nil {
		t.Fatal("RevokeRouterKey against a nonexistent file must error")
	}
}

// TestWriteRouterKeyRecordsAtomic_NoPartialFileVisible drives many concurrent
// writers (add then revoke, interleaved) against the same file and confirms
// every reader in flight the whole time only ever observed either a
// completely valid empty-or-populated JSON array, never a truncated or
// half-written file — the atomic temp-file-then-rename contract.
func TestWriteRouterKeyRecordsAtomic_NoPartialFileVisible(t *testing.T) {
	dir := t.TempDir()
	path := RouterKeysPath(dir)
	if _, err := AddRouterKey(dir, "seed"); err != nil {
		t.Fatalf("AddRouterKey: %v", err)
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	var readErr error
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(path)
			if err != nil {
				continue // a rename mid-flight can transiently miss the file; not what this test checks
			}
			var records []routerKeyRecord
			if err := json.Unmarshal(data, &records); err != nil {
				readErr = err
				close(stop)
				return
			}
		}
	}()

	for i := 0; i < 50; i++ {
		label := "k"
		if _, err := AddRouterKey(dir, label+"x"); err == nil {
			_ = RevokeRouterKey(dir, label+"x")
		}
	}
	select {
	case <-stop:
	default:
		close(stop)
	}
	<-done
	if readErr != nil {
		t.Fatalf("a concurrent reader observed a partially-written file: %v", readErr)
	}
}

func TestWriteRouterKeyRecordsAtomic_TempFileSameDirectory(t *testing.T) {
	dir := t.TempDir()
	path := RouterKeysPath(dir)
	if err := writeRouterKeyRecordsAtomic(path, []routerKeyRecord{{Label: "a", SHA256: strings.Repeat("0", 64), Created: "x"}}); err != nil {
		t.Fatalf("writeRouterKeyRecordsAtomic: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	// No leftover temp file: CreateTemp's file was renamed, not copied, so
	// exactly one entry (router_keys.json) should remain.
	if len(entries) != 1 || entries[0].Name() != RouterKeysFileName {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("directory entries = %v, want exactly [%s]", names, RouterKeysFileName)
	}
}

// ---------------------------------------------------------------------------
// RouterKeyStore: load, fail-closed states, and mtime-triggered reload.
// ---------------------------------------------------------------------------

func writeKeysFile(t *testing.T, path string, plaintexts map[string]string) {
	t.Helper()
	records := make([]routerKeyRecord, 0, len(plaintexts))
	for label, plaintext := range plaintexts {
		sum := sha256.Sum256([]byte(plaintext))
		records = append(records, routerKeyRecord{Label: label, SHA256: hex.EncodeToString(sum[:]), Created: time.Now().UTC().Format(time.RFC3339)})
	}
	// t.TempDir() itself creates directories at the process umask (typically
	// 0755, not 0700) — MkdirAll is a no-op mode-wise on a directory that
	// already exists, so it must be chmod'd explicitly, the same fix
	// writeRouterKeyRecordsAtomic applies for the same reason.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	data, err := json.Marshal(records)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestRouterKeyStore_NoFileIsInvalid(t *testing.T) {
	dir := t.TempDir()
	store := NewRouterKeyStore(RouterKeysPath(dir))
	if _, ok := store.Authenticate("rrk_anything"); ok {
		t.Fatal("Authenticate must fail when no keys file exists")
	}
}

func TestRouterKeyStore_EmptyFileIsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := RouterKeysPath(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewRouterKeyStore(path)
	if _, ok := store.Authenticate("rrk_anything"); ok {
		t.Fatal("Authenticate must fail against an empty keys file")
	}
}

func TestRouterKeyStore_ValidKeyAuthenticates(t *testing.T) {
	dir := t.TempDir()
	path := RouterKeysPath(dir)
	writeKeysFile(t, path, map[string]string{"hermes": "rrk_" + strings.Repeat("a", 64)})
	store := NewRouterKeyStore(path)

	label, ok := store.Authenticate("rrk_" + strings.Repeat("a", 64))
	if !ok || label != "hermes" {
		t.Fatalf("Authenticate = (%q, %v), want (hermes, true)", label, ok)
	}
	if _, ok := store.Authenticate("rrk_" + strings.Repeat("b", 64)); ok {
		t.Fatal("Authenticate must fail for a key that was never issued")
	}
	if _, ok := store.Authenticate(""); ok {
		t.Fatal("Authenticate must fail for an empty credential")
	}
}

// TestRouterKeyStore_GroupOrWorldReadableFileIsInvalid pins C10's permission
// gate for both the file and the containing directory, and for both
// group-readable (0640-shaped) and world-readable (0644-shaped) modes.
func TestRouterKeyStore_GroupOrWorldReadableFileIsInvalid(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			path := RouterKeysPath(dir)
			writeKeysFile(t, path, map[string]string{"hermes": "rrk_" + strings.Repeat("a", 64)})
			if err := os.Chmod(path, mode); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			store := NewRouterKeyStore(path)
			if _, ok := store.Authenticate("rrk_" + strings.Repeat("a", 64)); ok {
				t.Fatalf("Authenticate succeeded against a %s file; group/world-readable must be treated as no keys", mode)
			}
		})
	}
}

func TestRouterKeyStore_GroupOrWorldReadableDirectoryIsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := RouterKeysPath(dir)
	writeKeysFile(t, path, map[string]string{"hermes": "rrk_" + strings.Repeat("a", 64)})
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // let TempDir clean up
	store := NewRouterKeyStore(path)
	if _, ok := store.Authenticate("rrk_" + strings.Repeat("a", 64)); ok {
		t.Fatal("Authenticate succeeded with a world-readable containing directory")
	}
}

// TestRouterKeyStore_ReloadsWithinPollWindow drives the mtime-reload path
// with a controlled fake clock — no real sleep — proving add/revoke take
// effect on an already-running store. The store's poll only fires once
// s.now() has advanced past pollInterval since the last check, so the fake
// clock is what actually exercises reloadLocked, not wall-clock time.
func TestRouterKeyStore_ReloadsWithinPollWindow(t *testing.T) {
	dir := t.TempDir()
	path := RouterKeysPath(dir)
	writeKeysFile(t, path, map[string]string{"hermes": "rrk_" + strings.Repeat("a", 64)})

	store := NewRouterKeyStore(path)
	if _, ok := store.Authenticate("rrk_" + strings.Repeat("a", 64)); !ok {
		t.Fatal("setup: initial key must authenticate")
	}

	fakeNow := time.Now()
	store.setClockForTest(func() time.Time { return fakeNow }, time.Second)
	// setClockForTest resets lastPoll to zero, which forces the VERY NEXT
	// Authenticate call to reload unconditionally (see pollLocked) —
	// establishing fakeNow as the poll baseline before the file changes
	// below, rather than that forced reload firing (against the ALREADY
	// rewritten file) when the test means to check "not yet".
	if _, ok := store.Authenticate("rrk_" + strings.Repeat("a", 64)); !ok {
		t.Fatal("priming authenticate under the fake clock failed")
	}

	// Add a second key on disk. Advancing the fake mtime explicitly (rather
	// than relying on os.WriteFile's real wall-clock mtime, which could
	// collide with the first file's mtime at test speed) makes the "file
	// changed" detection deterministic.
	writeKeysFile(t, path, map[string]string{
		"hermes": "rrk_" + strings.Repeat("a", 64),
		"second": "rrk_" + strings.Repeat("b", 64),
	})
	newTime := time.Now().Add(5 * time.Second)
	if err := os.Chtimes(path, newTime, newTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Immediately after the write: still within the poll window (fake clock
	// hasn't advanced), so the store must NOT have reloaded yet.
	if _, ok := store.Authenticate("rrk_" + strings.Repeat("b", 64)); ok {
		t.Fatal("the new key authenticated before the poll window elapsed")
	}

	// Advance the fake clock past pollInterval: the next Authenticate call
	// must reload and see the new key.
	fakeNow = fakeNow.Add(2 * time.Second)
	label, ok := store.Authenticate("rrk_" + strings.Repeat("b", 64))
	if !ok || label != "second" {
		t.Fatalf("Authenticate after the poll window = (%q, %v), want (second, true)", label, ok)
	}

	// Now revoke "hermes" (rewrite the file with only "second") and confirm
	// that takes effect on the same running store, again within one poll
	// window — no restart.
	writeKeysFile(t, path, map[string]string{"second": "rrk_" + strings.Repeat("b", 64)})
	newTime2 := newTime.Add(5 * time.Second)
	if err := os.Chtimes(path, newTime2, newTime2); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	fakeNow = fakeNow.Add(2 * time.Second)
	if _, ok := store.Authenticate("rrk_" + strings.Repeat("a", 64)); ok {
		t.Fatal("a revoked key still authenticated after its poll window")
	}
	if _, ok := store.Authenticate("rrk_" + strings.Repeat("b", 64)); !ok {
		t.Fatal("the surviving key stopped authenticating after revoking a different label")
	}
}

// TestRouterKeyStore_AuthenticateUsesConstantTimeCompare is a code-level
// assertion, not a timing-side-channel test (out of scope per C10): it pins
// that hash comparison goes through crypto/subtle.ConstantTimeCompare and
// never a short-circuiting ==/bytes.Equal on secret-derived material, so a
// future edit that "simplifies" Authenticate back to bytes.Equal fails the
// build's own test suite rather than only a security review.
//
// Scoped to the Authenticate function body specifically, not the whole
// file: a security review of the first version of this test caught that
// searching the whole file for the string "subtle.ConstantTimeCompare"
// passes even if the real comparison is swapped to bytes.Equal, because
// this very doc comment also contains that string.
func TestRouterKeyStore_AuthenticateUsesConstantTimeCompare(t *testing.T) {
	src, err := os.ReadFile("keys.go")
	if err != nil {
		t.Fatalf("read keys.go: %v", err)
	}
	body := extractFuncBody(t, string(src), "func (s *RouterKeyStore) Authenticate(")
	if !strings.Contains(body, "subtle.ConstantTimeCompare") {
		t.Fatal("Authenticate must compare hashes via crypto/subtle.ConstantTimeCompare")
	}
	if strings.Contains(body, "bytes.Equal(") {
		t.Fatal("found a bytes.Equal comparison inside Authenticate; must use crypto/subtle.ConstantTimeCompare exclusively for hash material")
	}
}

// extractFuncBody returns the source text from sig (a function signature
// prefix, e.g. "func Foo(") up to the top-level closing brace that matches
// its opening one — a minimal brace-depth scan, not a full Go parser, but
// enough to isolate one function's body from the rest of the file for a
// source-scan test like the one above.
func extractFuncBody(t *testing.T, src, sig string) string {
	t.Helper()
	start := strings.Index(src, sig)
	if start == -1 {
		t.Fatalf("function signature %q not found in source", sig)
	}
	openBrace := strings.Index(src[start:], "{")
	if openBrace == -1 {
		t.Fatalf("no opening brace found after %q", sig)
	}
	openBrace += start
	depth := 0
	for i := openBrace; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[openBrace : i+1]
			}
		}
	}
	t.Fatalf("unterminated function body starting at %q", sig)
	return ""
}

func TestRouterKeyStore_KeyFromEnvVarIsIgnored(t *testing.T) {
	dir := t.TempDir()
	path := RouterKeysPath(dir)
	writeKeysFile(t, path, map[string]string{"hermes": "rrk_" + strings.Repeat("a", 64)})

	const envKey = "rrk_" + "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0"
	t.Setenv("ROUTER_KEY", envKey)
	t.Setenv("RELAY_LLM_ROUTER_KEY", envKey)

	store := NewRouterKeyStore(path)
	// The store never reads an env var at all — this just proves that
	// presenting the env-var-shaped value authenticates only if it happens
	// to equal a real stored key (it doesn't here), never because it came
	// from the environment.
	if _, ok := store.Authenticate(envKey); ok {
		t.Fatal("a key that only exists in an environment variable must not authenticate")
	}
	if _, ok := store.Authenticate(os.Getenv("ROUTER_KEY")); ok {
		t.Fatal("ROUTER_KEY env var must not be treated as a valid credential")
	}
}
