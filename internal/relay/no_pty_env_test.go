package relay_test

// L-S1 deleted relayLLM's PTY-env resolution (ResolvePtyEnv) along with the
// terminal/session/provider packages that were its only callers — this walks
// the module's own source tree and fails if any reference survived, rather
// than trusting a human glance.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoSourceReferencesResolvePtyEnv(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// This test file lives at <module root>/internal/relay.
	root := filepath.Join(wd, "..", "..")

	needle := []byte("ResolvePtyEnv")
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || filepath.Base(path) == "no_pty_env_test.go" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, needle) {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(offenders) > 0 {
		t.Errorf("found %d file(s) still referencing ResolvePtyEnv: %v", len(offenders), offenders)
	}
}
