package terminal

import (
	"strings"
	"testing"

	"relayllm/internal/sshhost"
)

// A host terminal never gets a project token or any relay secret in its argv
// (v1 carries no relay MCPs/tokens onto a host — decision 6).
func TestSec_HostTerminalExec_ArgvNeverContainsRelaySecrets(t *testing.T) {
	spec := hostTerminalSpec()
	for _, tmplID := range []string{"shell", "claude", "npm-test"} {
		_, argv := buildHostTerminalExec(spec, tmplID, "/proj", "npm", []string{"test"})
		decoded := sshhost.RemoteShellCommandDecodedForTest(argv[len(argv)-1])
		for _, secret := range []string{"RELAY_PROJECT_TOKEN", "RELAY_TOKEN", "RELAY_SERVICE_TOKEN", "RELAY_LLM_HOOK"} {
			if strings.Contains(decoded, secret) {
				t.Errorf("tmplID=%q host terminal script leaked %q: %s", tmplID, secret, decoded)
			}
		}
	}
}
