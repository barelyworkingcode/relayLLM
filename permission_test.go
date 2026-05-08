package main

import "testing"

func TestMatchToolRule(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		input    string
		patterns []string
		want     bool
	}{
		{"bare name match", "Read", `{"path":"/tmp/x"}`, []string{"Read"}, true},
		{"bare name miss", "Write", `{"path":"/tmp/x"}`, []string{"Read"}, false},
		{"prefix match", "Bash", `{"command":"ls -la"}`, []string{"Bash:\"command\":\"ls"}, true},
		{"prefix miss on tool name", "Read", `{"command":"ls"}`, []string{"Bash:ls"}, false},
		{"empty pattern list", "Read", "{}", nil, false},
		{"multiple patterns, second wins", "Glob", "{}", []string{"Read", "Glob"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchToolRule(tt.tool, tt.input, tt.patterns)
			if got != tt.want {
				t.Errorf("MatchToolRule(%q, %q, %v) = %v, want %v",
					tt.tool, tt.input, tt.patterns, got, tt.want)
			}
		})
	}
}
