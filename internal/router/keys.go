package router

// Standalone router keys (plan-broker-and-sessions.md §2 C10). A relayLLM
// run with RELAY_LAUNCH_FD unset has no relay to authenticate its TCP
// listener for it, so this is a small, self-contained API-key mechanism: a
// JSON file of label -> SHA-256(plaintext) pairs, a CLI to manage it
// (relayllm router-key add/list/revoke), and a request-time store
// (RouterKeyStore, auth.go) that polls the file's mtime instead of touching
// disk on every request.
//
// The plaintext key is never written anywhere but stdout, exactly once, at
// `add` time — router_keys.json holds only its hash, matching relay's own
// credential-at-rest convention.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// RouterKeysFileName is C10's on-disk file name, always directly under
// {dataDir}.
const RouterKeysFileName = "router_keys.json"

// RouterKeyPlaintextPrefix marks a standalone router key's plaintext form:
// this prefix followed by 64 lowercase hex characters (a 256-bit random
// value, hex-encoded).
const RouterKeyPlaintextPrefix = "rrk_"

// routerKeyLabelPattern is C10's label grammar. Enforced on `add`; a label
// already on disk that somehow doesn't match (hand-edited file) is still
// loaded and usable for authentication — only new labels are validated.
var routerKeyLabelPattern = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)

// routerKeyRecord is one entry of router_keys.json. The plaintext key is
// never a field here, on disk, or anywhere else past the moment `add`
// prints it — only its hash persists.
type routerKeyRecord struct {
	Label   string `json:"label"`
	SHA256  string `json:"sha256"`
	Created string `json:"created"`
}

// RouterKeysPath returns {dataDir}/router_keys.json.
func RouterKeysPath(dataDir string) string {
	return filepath.Join(dataDir, RouterKeysFileName)
}

// ---------------------------------------------------------------------------
// CLI-facing operations: add / list / revoke. Each is a single, atomic
// read-modify-write of the whole file — there is no concurrent-writer story
// beyond "don't run two CLI invocations at once", which matches relay's own
// credential file conventions.
// ---------------------------------------------------------------------------

// RouterKeyInfo is what `router-key list` shows: never the plaintext, never
// the full hash.
type RouterKeyInfo struct {
	Label      string
	Created    string
	HashPrefix string // first 12 hex characters of the stored SHA-256
}

// AddRouterKey generates a new key, appends its hash to {dataDir}'s
// router_keys.json (creating the file and directory, with the correct
// permissions, if this is the first key), and returns the plaintext. The
// plaintext is returned exactly once here and never persisted anywhere —
// the caller (the CLI) must print it and discard it.
func AddRouterKey(dataDir, label string) (plaintext string, err error) {
	if !routerKeyLabelPattern.MatchString(label) {
		return "", fmt.Errorf("label %q must match [a-z0-9._-]{1,64}", label)
	}
	path := RouterKeysPath(dataDir)
	records, err := readRouterKeyRecords(path)
	if err != nil {
		return "", err
	}
	for _, r := range records {
		if r.Label == label {
			return "", fmt.Errorf("a router key labeled %q already exists", label)
		}
	}

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("failed to generate key: %w", err)
	}
	plaintext = RouterKeyPlaintextPrefix + hex.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(plaintext))

	records = append(records, routerKeyRecord{
		Label:   label,
		SHA256:  hex.EncodeToString(sum[:]),
		Created: time.Now().UTC().Format(time.RFC3339),
	})
	if err := writeRouterKeyRecordsAtomic(path, records); err != nil {
		return "", err
	}
	return plaintext, nil
}

// ListRouterKeys reads every entry in {dataDir}/router_keys.json. A missing
// file is an empty list, not an error — mirroring the request-time store's
// "no file means no keys" reading, but this is an administrative read: it
// does not apply C10's group/world-readable-means-invalid rule, since that
// rule exists to fail closed on the request path, not to hide a label list
// from the operator who owns the file.
func ListRouterKeys(dataDir string) ([]RouterKeyInfo, error) {
	records, err := readRouterKeyRecords(RouterKeysPath(dataDir))
	if err != nil {
		return nil, err
	}
	infos := make([]RouterKeyInfo, 0, len(records))
	for _, r := range records {
		prefix := r.SHA256
		if len(prefix) > 12 {
			prefix = prefix[:12]
		}
		infos = append(infos, RouterKeyInfo{Label: r.Label, Created: r.Created, HashPrefix: prefix})
	}
	return infos, nil
}

// RevokeRouterKey removes label's entry from {dataDir}/router_keys.json. It
// is an error if the label does not exist, including when the file itself
// does not exist.
func RevokeRouterKey(dataDir, label string) error {
	path := RouterKeysPath(dataDir)
	records, err := readRouterKeyRecords(path)
	if err != nil {
		return err
	}
	idx := -1
	for i, r := range records {
		if r.Label == label {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("no router key labeled %q", label)
	}
	records = append(records[:idx], records[idx+1:]...)
	return writeRouterKeyRecordsAtomic(path, records)
}

// readRouterKeyRecords reads and parses path, treating a missing file or one
// containing only whitespace as zero records rather than an error — the
// natural "nothing configured yet" state for `add` (which then creates it)
// and `list`/`revoke` (which report accordingly).
func readRouterKeyRecords(path string) ([]routerKeyRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var records []routerKeyRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	return records, nil
}

// writeRouterKeyRecordsAtomic creates path's directory (0700) if needed,
// writes records to a temp file in that SAME directory (so the final rename
// is on one filesystem, never a copy), sets it to 0600, then renames it over
// path. A concurrent reader — the running server's RouterKeyStore polling
// this file on another request — only ever sees the old complete file or the
// new complete file, never a truncated one: rename(2) is atomic within a
// filesystem.
func writeRouterKeyRecordsAtomic(path string, records []routerKeyRecord) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create %s: %w", dir, err)
	}
	// MkdirAll only applies the mode when it actually creates the directory;
	// pin it down explicitly in case the directory pre-existed looser.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("failed to set permissions on %s: %w", dir, err)
	}

	if records == nil {
		records = []routerKeyRecord{}
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode router keys: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".router_keys-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to chmod %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to rename %s into place: %w", tmpPath, err)
	}
	renamed = true
	return nil
}

// ---------------------------------------------------------------------------
// RouterKeyStore: the request-serving side. One instance lives for the whole
// process lifetime of a standalone relayLLM; auth.go's middleware calls
// Authenticate on every request.
// ---------------------------------------------------------------------------

// routerKeyEntry is one loaded, ready-to-compare key: the label it
// authenticates as, and its decoded (raw, not hex) SHA-256.
type routerKeyEntry struct {
	label string
	hash  []byte // exactly sha256.Size bytes
}

// RouterKeyStore holds the currently valid set of router keys for one
// router_keys.json path, reloading from disk at most once per pollInterval
// (never once per request — see Authenticate). A missing file, an empty
// one, or one that (or whose containing directory) is group- or
// world-readable are all the same state: valid=false, meaning every request
// is refused regardless of what credential it presents.
type RouterKeyStore struct {
	path         string
	pollInterval time.Duration
	now          func() time.Time

	mu       sync.Mutex
	lastPoll time.Time
	modTime  time.Time
	valid    bool
	entries  []routerKeyEntry

	// warnedInvalid ensures the "no keys configured" state is logged once
	// per transition into that state (including the very first check, which
	// happens at construction, i.e. at process startup) rather than once per
	// poll for as long as the condition persists.
	warnedInvalid bool
}

// NewRouterKeyStore builds a store for path and performs its first load
// immediately (synchronously, before returning) — so the "no keys
// configured" warning, if applicable, is logged at startup, not deferred
// until whatever request happens to be first.
func NewRouterKeyStore(path string) *RouterKeyStore {
	s := &RouterKeyStore{path: path, pollInterval: time.Second, now: time.Now}
	s.mu.Lock()
	s.lastPoll = s.now()
	s.reloadLocked()
	s.mu.Unlock()
	return s
}

// setClockForTest replaces the wall clock and polling interval, and forces
// the next Authenticate call to reload immediately regardless of how long
// ago the real NewRouterKeyStore's eager load ran — the seam
// keys_test.go/auth_test.go use to exercise the mtime-reload path without a
// real sleep.
func (s *RouterKeyStore) setClockForTest(now func() time.Time, pollInterval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
	s.pollInterval = pollInterval
	s.lastPoll = time.Time{}
}

// Authenticate reports whether presented hashes to one of the currently
// loaded keys, and if so, which label it belongs to. presented is hashed
// once (SHA-256) and every stored hash is checked against it via
// crypto/subtle.ConstantTimeCompare — never a short-circuiting == or
// bytes.Equal on secret-derived material, and never a plaintext-to-plaintext
// comparison (there is no plaintext at rest to compare against). An empty
// presented value, or a store with no currently valid keys (see reloadLocked),
// always returns false.
func (s *RouterKeyStore) Authenticate(presented string) (label string, ok bool) {
	if presented == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pollLocked()
	if !s.valid {
		return "", false
	}
	sum := sha256.Sum256([]byte(presented))
	for _, e := range s.entries {
		if subtle.ConstantTimeCompare(sum[:], e.hash) == 1 {
			return e.label, true
		}
	}
	return "", false
}

// pollLocked re-stats the file at most once per pollInterval, reloading the
// in-memory key set only when that stat says something changed. Called with
// mu held.
func (s *RouterKeyStore) pollLocked() {
	now := s.now()
	if !s.lastPoll.IsZero() && now.Sub(s.lastPoll) < s.pollInterval {
		return
	}
	s.lastPoll = now
	s.reloadLocked()
}

// reloadLocked implements C10's fail-closed gate: a missing file, an empty
// one, or one (or a containing directory) that is group- or
// world-readable, are all folded into the same valid=false state as a parse
// failure or a file with zero usable entries. Called with mu held.
//
// Permission and existence checks run on every call (they're one os.Stat
// each, already paid for by the mtime check pollLocked is guarding), but the
// actual read+parse only runs when the file's mtime changed since the last
// successful load — the "at most once per second" cost C10 asks for is the
// stat, not a skipped permission check.
func (s *RouterKeyStore) reloadLocked() {
	fi, err := os.Stat(s.path)
	if err != nil {
		s.markInvalid(fmt.Sprintf("router keys file not present or unreadable: %s", s.path))
		return
	}
	if fi.Mode().Perm()&0o077 != 0 {
		s.markInvalid(fmt.Sprintf("router keys file %s is group- or world-readable (mode %s); refusing to treat any key in it as valid", s.path, fi.Mode().Perm()))
		return
	}
	dir := filepath.Dir(s.path)
	dirInfo, err := os.Stat(dir)
	if err != nil {
		s.markInvalid(fmt.Sprintf("router keys directory unreadable: %s", dir))
		return
	}
	if dirInfo.Mode().Perm()&0o077 != 0 {
		s.markInvalid(fmt.Sprintf("router keys directory %s is group- or world-readable (mode %s); refusing to treat any key in it as valid", dir, dirInfo.Mode().Perm()))
		return
	}
	if fi.Size() == 0 {
		s.markInvalid(fmt.Sprintf("router keys file is empty: %s", s.path))
		return
	}
	if s.valid && fi.ModTime().Equal(s.modTime) {
		return // permissions fine, content unchanged since the last good load
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		s.markInvalid(fmt.Sprintf("failed to read router keys file %s: %v", s.path, err))
		return
	}
	var records []routerKeyRecord
	if err := json.Unmarshal(data, &records); err != nil {
		s.markInvalid(fmt.Sprintf("router keys file %s is not valid JSON: %v", s.path, err))
		return
	}
	if len(records) == 0 {
		s.markInvalid(fmt.Sprintf("router keys file has no entries: %s", s.path))
		return
	}

	entries := make([]routerKeyEntry, 0, len(records))
	for _, rec := range records {
		hash, err := hex.DecodeString(rec.SHA256)
		if err != nil || len(hash) != sha256.Size {
			slog.Warn("router keys: skipping entry with an invalid hash", "label", rec.Label)
			continue
		}
		entries = append(entries, routerKeyEntry{label: rec.Label, hash: hash})
	}
	if len(entries) == 0 {
		s.markInvalid(fmt.Sprintf("router keys file has no usable entries: %s", s.path))
		return
	}

	s.entries = entries
	s.modTime = fi.ModTime()
	s.valid = true
	s.warnedInvalid = false
}

// markInvalid puts the store into the "no keys configured" state. It logs
// only on the transition into that state (see warnedInvalid's doc comment),
// so a persistently missing or misconfigured file logs once, not once per
// poll.
func (s *RouterKeyStore) markInvalid(reason string) {
	s.entries = nil
	s.modTime = time.Time{}
	s.valid = false
	if !s.warnedInvalid {
		slog.Warn("router keys: no keys configured; every standalone route, including /health, will return 401 until this is fixed", "reason", reason)
		s.warnedInvalid = true
	}
}
