package registry

import (
	"encoding/json"
	"testing"
)

func TestUpstreamModelRow_ContextLengthFieldNames(t *testing.T) {
	// Each server family advertises context under a different key; we read
	// whichever one is present so endpoint models get a real window.
	tests := []struct {
		name string
		body string
		want int64
	}{
		{"vLLM / OMLX", `{"id":"m","max_model_len":262144}`, 262144},
		{"LM Studio", `{"id":"m","max_context_length":32768}`, 32768},
		{"context_length", `{"id":"m","context_length":16384}`, 16384},
		{"context_window", `{"id":"m","context_window":8192}`, 8192},
		{"llama.cpp meta", `{"id":"m","meta":{"n_ctx":4096}}`, 4096},
		{"meta n_ctx_train fallback", `{"id":"m","meta":{"n_ctx_train":2048}}`, 2048},
		{"none advertised", `{"id":"m"}`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var row upstreamModelRow
			if err := json.Unmarshal([]byte(tc.body), &row); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := row.contextLength(); got != tc.want {
				t.Errorf("contextLength() = %d, want %d", got, tc.want)
			}
		})
	}
}
