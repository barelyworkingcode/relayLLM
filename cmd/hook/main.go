package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

func logDebug(format string, args ...interface{}) {
	f, err := os.OpenFile("/tmp/relay-hook.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	logger := log.New(f, "", log.LstdFlags)
	logger.Printf(format, args...)
}

func main() {
	hookURL := os.Getenv("RELAY_LLM_HOOK_URL")
	sessionID := os.Getenv("RELAY_LLM_SESSION_ID")
	headless := os.Getenv("RELAY_LLM_HEADLESS")

	logDebug("hook invoked: hookURL=%q sessionID=%q headless=%q", hookURL, sessionID, headless)

	// No-op when not running under relayLLM.
	if hookURL == "" {
		logDebug("no hookURL, exiting 0")
		os.Exit(0)
	}

	// Headless sessions auto-approve all tool use — no human in the loop.
	// Must output an explicit "allow" decision so Claude Code bypasses all
	// permission checks including path-level restrictions (e.g. .claude/ dir).
	// A silent exit(0) only means "no opinion" and defers to built-in checks.
	if headless == "true" {
		output, _ := json.Marshal(map[string]interface{}{
			"hookSpecificOutput": map[string]string{
				"hookEventName":            "PreToolUse",
				"permissionDecision":       "allow",
				"permissionDecisionReason": "headless session — all tools auto-approved",
			},
		})
		logDebug("headless allow: %s", string(output))
		fmt.Println(string(output))
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

	// Interactive UI tools have no side effects — skip the permission roundtrip.
	switch data.ToolName {
	case "ExitPlanMode", "AskUserQuestion":
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
