package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	hookURL := os.Getenv("RELAY_LLM_HOOK_URL")
	sessionID := os.Getenv("RELAY_LLM_SESSION_ID")

	// No-op when not running under relayLLM.
	if hookURL == "" {
		os.Exit(0)
	}

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(0)
	}

	var data struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
		ToolUseID string          `json:"tool_use_id"`
	}
	if err := json.Unmarshal(input, &data); err != nil {
		os.Exit(0)
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"sessionId": sessionID,
		"toolName":  data.ToolName,
		"toolInput": string(data.ToolInput),
		"toolUseId": data.ToolUseID,
	})

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Post(
		fmt.Sprintf("%s/api/permission", hookURL),
		"application/json",
		bytes.NewReader(payload),
	)
	if err != nil {
		// Network error — fail-open.
		os.Exit(0)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		os.Exit(0)
	}

	var result struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		os.Exit(0)
	}

	output, _ := json.Marshal(map[string]interface{}{
		"hookSpecificOutput": map[string]string{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       result.Decision,
			"permissionDecisionReason": result.Reason,
		},
	})
	fmt.Println(string(output))
}
