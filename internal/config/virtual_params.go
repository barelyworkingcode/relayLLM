package config

import (
	"bytes"
	"fmt"
)

// ValidateVirtualParams fails on the first virtual target whose params is
// present but not a JSON object. Absent, null (read as absent) and {} pass.
// Keys are not checked: whether an upstream accepts a field is its business.
func ValidateVirtualParams(cfg *VirtualLLMConfig) error {
	if cfg == nil {
		return nil
	}
	for _, v := range cfg.Models {
		for i, t := range v.Targets {
			raw := bytes.TrimSpace(t.Params)
			if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
				continue
			}
			if raw[0] == '{' {
				continue // json.Unmarshal already proved it well-formed
			}
			return fmt.Errorf("virtual-llms %q targets[%d] (%s): params must be a JSON object, got %s", v.Name, i, t.Label(), jsonKind(raw))
		}
	}
	return nil
}

func jsonKind(raw []byte) string {
	switch raw[0] {
	case '"':
		return "string"
	case '[':
		return "array"
	case 't', 'f':
		return "boolean"
	default:
		return "number"
	}
}
