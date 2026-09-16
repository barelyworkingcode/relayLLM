package api

import (
	"encoding/json"
	"relayllm/internal/servermanager"
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
		UptimeSeconds int64                              `json:"uptimeSeconds"`
		Instances     []servermanager.ServerInstanceInfo `json:"instances"`
		MlxInstances  []servermanager.ServerInstanceInfo `json:"mlxInstances"`
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
	if got.MlxInstances == nil {
		t.Fatal("mlxInstances field missing or null; want empty array []")
	}

	// Pull the raw JSON to confirm no stale `llamaInstances` count field
	// leaks through, and that relayLLM no longer reports session/terminal
	// counts on this payload now that it hosts neither (L-S1).
	var raw map[string]json.RawMessage
	srv.GetJSON("/api/status", &raw)
	if _, hasOldField := raw["llamaInstances"]; hasOldField {
		t.Error("status payload still carries deprecated llamaInstances field")
	}
	if _, hasInstances := raw["instances"]; !hasInstances {
		t.Error("status payload missing instances array")
	}
	if _, hasMlx := raw["mlxInstances"]; !hasMlx {
		t.Error("status payload missing mlxInstances array")
	}
	for _, stale := range []string{"sessions", "terminals"} {
		if _, has := raw[stale]; has {
			t.Errorf("status payload still carries %q; relayLLM no longer hosts sessions/terminals", stale)
		}
	}
}

func TestAPI_Status_MlxRoutes_NoManager(t *testing.T) {
	srv := NewTestServer(t, nil)

	// GET /api/mlx/instances should return empty array when no mlx manager.
	var got []servermanager.ServerInstanceInfo
	resp := srv.GetJSON("/api/mlx/instances", &got)
	if resp.StatusCode != 200 {
		t.Fatalf("mlx instances: got %d, want 200", resp.StatusCode)
	}
	if len(got) != 0 {
		t.Errorf("expected empty mlx instances, got %d", len(got))
	}

	// DELETE /api/mlx/instances/foo should 404 when no mlx manager.
	resp = srv.DeleteJSON("/api/mlx/instances/foo", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("mlx stop: got %d, want 404", resp.StatusCode)
	}
}
