package main

import (
	"encoding/json"
	"testing"
)

// The status payload is part of the wire contract relay's UI consumes.
// Field names + the embedded "instances" array are not free to rename —
// the manifest's stop-llama action declares `forEach: "instances"` and
// substitutes {alias} from row entries. If a field below changes, the
// matching manifest declaration (manifest.go) needs the corresponding
// rename in lockstep.
func TestAPI_Status_ShapeMatchesManifestForEach(t *testing.T) {
	srv := NewTestServer(t, nil)

	var got struct {
		UptimeSeconds int64               `json:"uptimeSeconds"`
		Sessions      int                 `json:"sessions"`
		Instances     []LlamaInstanceInfo `json:"instances"`
		Terminals     []TerminalSummary   `json:"terminals"`
	}
	resp := srv.GetJSON("/api/status", &got)
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	// Both arrays must be present as arrays, even when empty — the UI
	// renders forEach buttons by iterating them. nil/missing would crash JS.
	if got.Instances == nil {
		t.Fatal("instances field missing or null; want empty array []")
	}
	if got.Terminals == nil {
		t.Fatal("terminals field missing or null; want empty array []")
	}

	// Pull the raw JSON to confirm no stale `llamaInstances` count field
	// leaks through (consumers of the old shape should break loudly), and
	// that the new `terminals` field is an array not a count.
	var raw map[string]json.RawMessage
	srv.GetJSON("/api/status", &raw)
	if _, hasOldField := raw["llamaInstances"]; hasOldField {
		t.Error("status payload still carries deprecated llamaInstances field")
	}
	if _, hasInstances := raw["instances"]; !hasInstances {
		t.Error("status payload missing instances array")
	}
	termRaw, hasTerminals := raw["terminals"]
	if !hasTerminals {
		t.Error("status payload missing terminals array")
	}
	// Guard against the previous int-count shape sneaking back in.
	if len(termRaw) > 0 && termRaw[0] != '[' {
		t.Errorf("terminals must be an array (was a count?); got %s", string(termRaw))
	}
}
