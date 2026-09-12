package types

// HostSpec mirrors relay's bridge HostSpec (../relay/docs/ssh-hosts.md):
// the resolved ssh argv prefix and absolute tool paths relay's probe found on
// an SSH host project. relayLLM never derives ssh arguments itself — relay
// owns the one translation from a host record to argv (decision 2).
type HostSpec struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	SSHArgv    []string `json:"ssh_argv"`
	NodePath   string   `json:"node_path,omitempty"`
	ClaudePath string   `json:"claude_path,omitempty"`
	Shell      string   `json:"shell,omitempty"`
	OS         string   `json:"os,omitempty"`
}

// Chip returns the {id, name} summary a terminal event carries — never the
// full spec (ssh_argv, tool paths) to a surface that only needs a label.
// nil-safe: a console (non-host) terminal's nil Host chips to nil.
func (h *HostSpec) Chip() map[string]string {
	if h == nil {
		return nil
	}
	return map[string]string{"id": h.ID, "name": h.Name}
}
