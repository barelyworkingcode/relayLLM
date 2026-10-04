package relay_test

import (
	"testing"

	"relayllm/internal/relay"
)

// The settings editor must offer `params` on each virtual-llms target.
func TestManifest_SchemaDeclaresVirtualTargetParams(t *testing.T) {
	var targetFields []relay.FieldDecl
	for _, s := range relay.BuildManifest(t.TempDir()).Config.Schema {
		if s.ID != "virtual-llms" {
			continue
		}
		for _, f := range s.Fields {
			if f.ID != "models" || f.Item == nil {
				continue
			}
			for _, g := range f.Item.Fields {
				if g.ID == "targets" && g.Item != nil {
					targetFields = g.Item.Fields
				}
			}
		}
	}
	for _, f := range targetFields {
		if f.ID == "params" {
			if f.Type != "json" {
				t.Errorf("params type = %q, want json", f.Type)
			}
			return
		}
	}
	t.Fatal("virtual-llms targets have no params field")
}
