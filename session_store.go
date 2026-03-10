package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
