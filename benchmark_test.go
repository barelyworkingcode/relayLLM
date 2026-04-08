package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type benchTurnStats struct {
	TTFT            float64
	Total           time.Duration
	OutTokens       int
	PromptEvalCount int
}

type benchResult struct {
	Name         string
	Turns        []benchTurnStats
	FinalTTFT    float64
	FinalTotal   time.Duration
	FinalTPS     float64
	InputTokens  int
	OutputTokens int
	PromptEval   int
	Response     string
}

type benchCase struct {
	name         string
	provider     string
	model        string
	backendURL   string
	backendOK    bool
	settings     map[string]interface{}
	systemPrompt string
}

// TestBenchmark_OllamaProvider benchmarks Ollama provider with and without
// thinking, tracking per-turn KV cache diagnostics and context recall.
//
// Run with: go test -v -run TestBenchmark_OllamaProvider -timeout 30m
// Requires a running Ollama instance.
func TestBenchmark_OllamaProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping benchmark (requires running Ollama)")
	}

	ollamaURL := os.Getenv("OLLAMA_URL")
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}

	ollamaModel := os.Getenv("BENCHMARK_OLLAMA_MODEL")
	if ollamaModel == "" {
		ollamaModel = "gemma4"
	}

	ollamaOK := checkEndpoint(ollamaURL+"/api/tags", "")
	if !ollamaOK {
		t.Skip("Ollama is not reachable")
	}

	// Conversation turns. Turn 0 seeds a memory word for later recall.
	conversationTurns := []string{
		"Remember the word 'gopher'. I will ask you about it later. For now just reply 'Understood.'",
		"What is a goroutine in Go? 2 sentences max.",
		"How does a channel differ from a mutex? 2 sentences max.",
		"What is the select statement used for? 2 sentences.",
		"Explain context.Context in one sentence.",
		"What is defer used for? One sentence.",
		"What is an interface in Go? 2 sentences max.",
		"How do you handle errors in Go? 2 sentences max.",
		"What is a slice vs an array? 2 sentences.",
		"Explain the init function in one sentence.",
		"What does go mod tidy do? One sentence.",
		"What is a struct tag used for? 2 sentences max.",
		"How does type embedding work? 2 sentences max.",
		"What is a goroutine leak? 2 sentences.",
		"Explain sync.WaitGroup in one sentence.",
		"What is the blank identifier used for? One sentence.",
		"What is a pointer receiver vs value receiver? 2 sentences max.",
		"How does the range keyword work? 2 sentences.",
	}
	finalQuestion := "What was the special word I asked you to remember at the start of our conversation? Reply with just that word and nothing else."

	cases := []benchCase{
		{
			name: "ollama", provider: "ollama", model: ollamaModel,
			backendURL: ollamaURL, backendOK: ollamaOK,
			settings: map[string]interface{}{"temperature": 0.1, "top_p": 0.9, "min_p": 0.05},
		},
		{
			name: "ollama+thinking", provider: "ollama", model: ollamaModel,
			backendURL: ollamaURL, backendOK: ollamaOK,
			settings: map[string]interface{}{"temperature": 0.1, "top_p": 0.9, "min_p": 0.05, "think": true},
		},
	}

	var results []benchResult

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.backendOK {
				t.Skipf("backend not reachable for %s", tc.name)
			}

			ts := newTestServer(t)
			ts.Sessions.SetOllamaURL(ollamaURL)

			settingsJSON, _ := json.Marshal(tc.settings)

			session, err := ts.Sessions.CreateSession(
				"", t.TempDir(), "bench-"+tc.name,
				tc.model, tc.systemPrompt, false, tc.provider, settingsJSON,
			)
			if err != nil {
				t.Fatalf("create session: %v", err)
			}

			// Build up conversation, tracking per-turn stats.
			turns := make([]benchTurnStats, len(conversationTurns))
			for i, msg := range conversationTurns {
				start := time.Now()
				response, stats, err := ts.Sessions.SendMessageSync(session.ID, msg, nil)
				elapsed := time.Since(start)
				if err != nil {
					t.Fatalf("turn %d failed: %v", i, err)
				}
				if response == "" {
					t.Fatalf("empty response at turn %d", i)
				}
				turns[i] = benchTurnStats{
					TTFT:            stats.TimeToFirstToken,
					Total:           elapsed,
					OutTokens:       stats.OutputTokens,
					PromptEvalCount: stats.PromptEvalCount,
				}
				t.Logf("turn %d: TTFT=%.3fs total=%.3fs out=%d prompt_eval=%d | %s → %s",
					i, stats.TimeToFirstToken, elapsed.Seconds(), stats.OutputTokens,
					stats.PromptEvalCount, truncate(msg, 40), truncate(response, 60))
			}

			// Measure the final turn.
			start := time.Now()
			response, stats, err := ts.Sessions.SendMessageSync(session.ID, finalQuestion, nil)
			totalTime := time.Since(start)

			if err != nil {
				t.Fatalf("final message: %v", err)
			}

			t.Logf("final: TTFT=%.3fs total=%.3fs tps=%.1f prompt_eval=%d | %s",
				stats.TimeToFirstToken, totalTime.Seconds(), stats.TokensPerSecond,
				stats.PromptEvalCount, truncate(response, 80))

			// Validate context was maintained.
			if !strings.Contains(strings.ToLower(response), "gopher") {
				t.Errorf("context lost: expected 'gopher', got: %s", response)
			}

			results = append(results, benchResult{
				Name:         tc.name,
				Turns:        turns,
				FinalTTFT:    stats.TimeToFirstToken,
				FinalTotal:   totalTime,
				FinalTPS:     stats.TokensPerSecond,
				InputTokens:  stats.InputTokens,
				OutputTokens: stats.OutputTokens,
				PromptEval:   stats.PromptEvalCount,
				Response:     response,
			})
		})
	}

	if len(results) == 0 {
		return
	}

	// Print per-turn TTFT table.
	fmt.Println()
	fmt.Println("=== Per-Turn TTFT (seconds) ===")
	printBenchHeader(results)
	for i := range conversationTurns {
		fmt.Printf("%-25s", fmt.Sprintf("turn %d", i))
		for _, r := range results {
			fmt.Printf(" | %18.3f", r.Turns[i].TTFT)
		}
		fmt.Println()
	}
	fmt.Printf("%-25s", "final (recall)")
	for _, r := range results {
		fmt.Printf(" | %18.3f", r.FinalTTFT)
	}
	fmt.Println()

	// Print per-turn total time table.
	fmt.Println()
	fmt.Println("=== Per-Turn Total Time (seconds) ===")
	printBenchHeader(results)
	for i := range conversationTurns {
		fmt.Printf("%-25s", fmt.Sprintf("turn %d", i))
		for _, r := range results {
			fmt.Printf(" | %18.3f", r.Turns[i].Total.Seconds())
		}
		fmt.Println()
	}
	fmt.Printf("%-25s", "final (recall)")
	for _, r := range results {
		fmt.Printf(" | %18.3f", r.FinalTotal.Seconds())
	}
	fmt.Println()

	// Print per-turn prompt_eval_count table.
	fmt.Println()
	fmt.Println("=== Per-Turn Prompt Eval Count ===")
	printBenchHeader(results)
	for i := range conversationTurns {
		fmt.Printf("%-25s", fmt.Sprintf("turn %d", i))
		for _, r := range results {
			fmt.Printf(" | %18d", r.Turns[i].PromptEvalCount)
		}
		fmt.Println()
	}
	fmt.Printf("%-25s", "final (recall)")
	for _, r := range results {
		fmt.Printf(" | %18d", r.PromptEval)
	}
	fmt.Println()

	// Print summary table.
	fmt.Println()
	fmt.Println("=== Final Turn Summary ===")
	fmt.Printf("%-25s | %8s | %9s | %7s | %8s | %8s | %10s\n",
		"Config", "TTFT (s)", "Total (s)", "Tok/s", "In Tok", "Out Tok", "PromptEval")
	fmt.Printf("%-25s-|-%8s-|-%9s-|-%7s-|-%8s-|-%8s-|-%10s\n",
		strings.Repeat("-", 25), "--------", "---------", "-------",
		"--------", "--------", "----------")
	for _, r := range results {
		fmt.Printf("%-25s | %8.3f | %9.3f | %7.1f | %8d | %8d | %10d\n",
			r.Name, r.FinalTTFT, r.FinalTotal.Seconds(), r.FinalTPS,
			r.InputTokens, r.OutputTokens, r.PromptEval)
	}
	fmt.Println()
}

func printBenchHeader(results []benchResult) {
	fmt.Printf("%-25s", "Turn")
	for _, r := range results {
		fmt.Printf(" | %18s", r.Name)
	}
	fmt.Println()
	fmt.Printf("%-25s", strings.Repeat("-", 25))
	for range results {
		fmt.Printf("-|-%18s", strings.Repeat("-", 18))
	}
	fmt.Println()
}

func checkEndpoint(url, authHeader string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false
	}
	if authHeader != "" && authHeader != "Bearer " {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
