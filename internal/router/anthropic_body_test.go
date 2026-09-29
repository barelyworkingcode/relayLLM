package router

import "testing"

func TestMutateAnthropicBody_SplicesOnlyTopLevelModel(t *testing.T) {
	cases := []struct {
		name, in, model, want string
	}{
		{
			name:  "odd whitespace, escapes and html characters kept",
			in:    " {\n\t\"model\" :  \"claude-x\" ,\"system\":\"a \\u003c b <c> & \\\"q\\\" \\u00e9\",\"n\": 1.50e0 }\r\n",
			model: "host/Qwen3.8 27B Code",
			want:  " {\n\t\"model\" :  \"host/Qwen3.8 27B Code\" ,\"system\":\"a \\u003c b <c> & \\\"q\\\" \\u00e9\",\"n\": 1.50e0 }\r\n",
		},
		{
			name:  "escaped original value",
			in:    `{"messages":[],"model":"cl\u0061ude-\"x\""}`,
			model: "m",
			want:  `{"messages":[],"model":"m"}`,
		},
		{
			name:  "nested model keys and model-like text untouched",
			in:    `{"metadata":{"model":"keep"},"messages":[{"model":"keep2","content":"\"model\":\"x\""}],"model":"a","tools":[{"model":"keep3"}]}`,
			model: "b",
			want:  `{"metadata":{"model":"keep"},"messages":[{"model":"keep2","content":"\"model\":\"x\""}],"model":"b","tools":[{"model":"keep3"}]}`,
		},
		{
			name:  "duplicate top-level model leaves no client value behind",
			in:    `{"model":"a","x":1,"model":"c"}`,
			model: "b",
			want:  `{"model":"b","x":1,"model":"b"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mutateAnthropicBody([]byte(tc.in), anthropicMutation{Model: tc.model})
			if err != nil {
				t.Fatalf("mutateAnthropicBody: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestMutateAnthropicBody_RejectsUnusableBodies(t *testing.T) {
	cases := map[string]string{
		"invalid json":  `{"model":"a",`,
		"empty":         ``,
		"array":         `[{"model":"a"}]`,
		"bare string":   `"model"`,
		"missing model": `{"messages":[{"model":"nested-only"}]}`,
		"numeric model": `{"model":7}`,
		"null model":    `{"model":null}`,
		"object model":  `{"model":{"id":"a"}}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := mutateAnthropicBody([]byte(in), anthropicMutation{Model: "b"}); err == nil {
				t.Errorf("want error, got body %q", got)
			}
		})
	}
}
