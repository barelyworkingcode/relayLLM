#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${1:-http://localhost:3001}"
MODEL="mlx-community/qwen3.5-35b-a3b"

echo "=== MCP Integration Test ==="
echo "Target: $BASE_URL"
echo "Model: $MODEL"
echo ""

# Create project with MCP integrations.
echo "--- Creating project with integrations ---"
PROJECT=$(curl -sf -X POST "$BASE_URL/api/projects" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "mcp-test",
    "path": "/tmp",
    "model": "'"$MODEL"'",
    "integrations": ["mcp/relay"]
  }')
PROJECT_ID=$(echo "$PROJECT" | jq -r '.id')
echo "Project: $PROJECT_ID"
echo "Integrations: $(echo "$PROJECT" | jq -c '.integrations')"
echo ""

# Create session on that project.
echo "--- Creating session ---"
SESSION=$(curl -sf -X POST "$BASE_URL/api/sessions" \
  -H 'Content-Type: application/json' \
  -d "{\"projectId\":\"$PROJECT_ID\"}")
SESSION_ID=$(echo "$SESSION" | jq -r '.sessionId')
echo "Session: $SESSION_ID"
echo ""

# Ask it to list MCP tools — this should trigger the model to inspect available tools.
echo "--- Sending message: list available MCP tools ---"
RESULT=$(curl -sf --max-time 120 -X POST "$BASE_URL/api/sessions/$SESSION_ID/message" \
  -H 'Content-Type: application/json' \
  -d '{"text":"List all available MCP tools you have access to. Just list their names and a one-line description for each."}')

echo "Response:"
echo "$RESULT" | jq -r '.response'
echo ""
echo "Stats:"
echo "$RESULT" | jq '.stats'
echo ""

# Cleanup.
echo "--- Cleaning up ---"
curl -sf -X DELETE "$BASE_URL/api/sessions/$SESSION_ID" > /dev/null
echo "Deleted session $SESSION_ID"
curl -sf -X DELETE "$BASE_URL/api/projects/$PROJECT_ID" > /dev/null
echo "Deleted project $PROJECT_ID"

echo ""
echo "=== Done ==="
