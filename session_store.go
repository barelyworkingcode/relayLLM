package main

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *SessionStore) Save(session *Session) error {
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}

	path := filepath.Join(s.dir, session.ID+".json")
	return os.WriteFile(path, data, 0600)
}

func (s *SessionStore) Load(id string) (*Session, error) {
	path := filepath.Join(s.dir, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

func (s *SessionStore) Delete(id string) error {
	path := filepath.Join(s.dir, id+".json")
	return os.Remove(path)
}

func (s *SessionStore) LoadAll() ([]*Session, error) {
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}

	var sessions []*Session
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		session, err := s.Load(id)
		if err != nil {
			continue
		}
		sessions = append(sessions, session)
	}
	return sessions, nil
}

// sweepSessions does a single pass over sessions/: deletes headless session
// files older than headlessMaxAge, and returns the set of piSessionIds
// referenced by surviving sessions (so the pi sweeper can skip live ones).
// Non-headless sessions are never deleted — only the UI removes those.
func sweepSessions(dir string, headlessMaxAge time.Duration) (removed int, livePi map[string]struct{}, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, map[string]struct{}{}, nil
		}
		return 0, nil, err
	}
	livePi = make(map[string]struct{})
	cutoff := time.Now().Add(-headlessMaxAge)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s struct {
			ID            string `json:"sessionId"`
			Headless      bool   `json:"headless"`
			ProviderState struct {
				PiSessionID string `json:"piSessionId"`
			} `json:"providerState"`
		}
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}
		info, infoErr := entry.Info()
		if s.Headless && infoErr == nil && info.ModTime().Before(cutoff) {
			if rmErr := os.Remove(path); rmErr != nil {
				slog.Warn("session sweep: remove failed", "id", s.ID, "error", rmErr)
				continue
			}
			removed++
			continue
		}
		if s.ProviderState.PiSessionID != "" {
			livePi[s.ProviderState.PiSessionID] = struct{}{}
		}
	}
	return removed, livePi, nil
}

// sweepOrphanedPiSessions deletes pi-session JSONLs whose piSessionId isn't
// in livePi. The minAge cushion avoids racing a pi process whose owning
// session.json hasn't yet persisted its piSessionId. Walks recursively to
// cover both pi's flat ({dir}/<ts>_<uuid>.jsonl) and nested
// ({dir}/<cwd>/<ts>_<uuid>.jsonl) layouts.
func sweepOrphanedPiSessions(piDir string, livePi map[string]struct{}, minAge time.Duration) (removed int, err error) {
	cutoff := time.Now().Add(-minAge)
	walkErr := filepath.WalkDir(piDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().After(cutoff) {
			return nil
		}
		base := strings.TrimSuffix(d.Name(), ".jsonl")
		idx := strings.LastIndex(base, "_")
		if idx < 0 {
			return nil
		}
		piID := base[idx+1:]
		if _, live := livePi[piID]; live {
			return nil
		}
		if rmErr := os.Remove(path); rmErr != nil {
			slog.Warn("orphan pi session: remove failed", "path", path, "error", rmErr)
			return nil
		}
		slog.Info("orphan pi session removed", "path", path, "piSessionId", piID)
		removed++
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		return removed, walkErr
	}
	return removed, nil
}
