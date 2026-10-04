package config

// Coverage for relayLLM#25 L2 criterion 10: an endpoint baseURL that carries
// userinfo or a query-string key never reaches a log line or a returned error.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	urlSecretPW  = "pw-Zx81qSecret"
	urlSecretKey = "key-Zx81qSecret"
)

func loadWithEndpointURL(t *testing.T, baseURL string, allowPlaintext bool) (logs string, err error) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	settings := map[string]any{
		"allowPlaintextEndpoints": allowPlaintext,
		"openai": map[string]any{
			"endpoints": []map[string]any{{"name": "acme", "baseURL": baseURL}},
		},
	}
	raw, _ := json.Marshal(settings)
	dir := t.TempDir()
	if werr := os.WriteFile(filepath.Join(dir, "settings.json"), raw, 0o600); werr != nil {
		t.Fatal(werr)
	}
	_, err = LoadConfig(dir, "")
	return buf.String(), err
}

func assertNoSecrets(t *testing.T, what, text string) {
	t.Helper()
	for _, s := range []string{urlSecretPW, urlSecretKey} {
		if strings.Contains(text, s) {
			t.Errorf("%s leaks %q: %s", what, s, text)
		}
	}
}

func TestEndpointURL_CredentialsNeverLogged(t *testing.T) {
	const host = "models.acme.example"
	withCreds := "http://svc:" + urlSecretPW + "@" + host + "/v1?api_key=" + urlSecretKey

	t.Run("plaintext allowed warning", func(t *testing.T) {
		logs, err := loadWithEndpointURL(t, withCreds, true)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		assertNoSecrets(t, "log", logs)
		if !strings.Contains(logs, host) {
			t.Errorf("warning no longer names the host %s: %s", host, logs)
		}
	})

	t.Run("plaintext rejected error", func(t *testing.T) {
		logs, err := loadWithEndpointURL(t, withCreds, false)
		if err == nil {
			t.Fatal("want error for plain http to a non-loopback host")
		}
		assertNoSecrets(t, "error", err.Error())
		assertNoSecrets(t, "log", logs)
		if !strings.Contains(err.Error(), host) {
			t.Errorf("error no longer names the host %s: %v", host, err)
		}
	})

	t.Run("unparseable URL error", func(t *testing.T) {
		bad := "http://svc:" + urlSecretPW + "@" + host + ":80x/v1?api_key=" + urlSecretKey
		logs, err := loadWithEndpointURL(t, bad, true)
		if err == nil {
			t.Fatal("want error for an unparseable baseURL")
		}
		assertNoSecrets(t, "error", err.Error())
		assertNoSecrets(t, "log", logs)
	})
}
