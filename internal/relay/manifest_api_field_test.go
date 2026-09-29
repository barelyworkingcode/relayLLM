package relay_test

import (
	"testing"

	"relayllm/internal/relay"
)

// The settings editor must offer `api` wherever settings.json accepts it:
// each openai endpoint and the three managed-server sections.
func TestManifest_SchemaDeclaresAPIField(t *testing.T) {
	schema := relay.BuildManifest(t.TempDir()).Config.Schema
	section := func(id string) []relay.FieldDecl {
		for _, f := range schema {
			if f.ID == id {
				return f.Fields
			}
		}
		t.Fatalf("schema has no %q section", id)
		return nil
	}
	var endpointItem []relay.FieldDecl
	for _, f := range section("openai") {
		if f.ID == "endpoints" && f.Item != nil {
			endpointItem = f.Item.Fields
		}
	}

	for where, fields := range map[string][]relay.FieldDecl{
		"openai.endpoints[]": endpointItem,
		"llama-server":       section("llama-server"),
		"mlx-serve":          section("mlx-serve"),
		"splash-serve":       section("splash-serve"),
	} {
		found := false
		for _, f := range fields {
			if f.ID == "api" {
				found = true
				if f.Type != "string[]" || f.Label != "APIs served" {
					t.Errorf("%s api = {type %q, label %q}, want {string[], APIs served}", where, f.Type, f.Label)
				}
			}
		}
		if !found {
			t.Errorf("%s has no api field", where)
		}
	}
}
