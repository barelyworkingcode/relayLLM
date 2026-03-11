#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${1:-http://localhost:3001}"

echo "=== relayLLM smoke test ==="
echo "Target: $BASE_URL"
echo ""

# Check connectivity.
echo "--- Checking models endpoint ---"
curl -sf "$BASE_URL/api/models" | jq .
echo ""

# Create project.
echo "--- Creating project ---"
PROJECT=$(curl -sf -X POST "$BASE_URL/api/projects" \
  -H 'Content-Type: application/json' \
  -d '{"name":"smoketest","path":"/tmp","model":"haiku"}')
PROJECT_ID=$(echo "$PROJECT" | jq -r '.id')
echo "Project ID: $PROJECT_ID"
echo ""

# Create session.
echo "--- Creating session ---"
SESSION=$(curl -sf -X POST "$BASE_URL/api/sessions" \
  -H 'Content-Type: application/json' \
  -d "{\"projectId\":\"$PROJECT_ID\",\"name\":\"smoketest session\"}")
SESSION_ID=$(echo "$SESSION" | jq -r '.sessionId')
echo "Session ID: $SESSION_ID"
echo ""

# Send message (sync — blocks until response).
echo "--- Sending message ---"
RESULT=$(curl -sf -X POST "$BASE_URL/api/sessions/$SESSION_ID/message" \
  -H 'Content-Type: application/json' \
  -d '{"text":"Respond with exactly: hello world"}')
RESPONSE=$(echo "$RESULT" | jq -r '.response')
COST=$(echo "$RESULT" | jq -r '.stats.costUsd')
echo "Response: $RESPONSE"
echo "Cost: \$$COST"
echo ""

# Validate response.
if echo "$RESPONSE" | grep -qi "hello"; then
  echo "PASS: response contains 'hello'"
else
  echo "FAIL: expected 'hello' in response"
fi
echo ""

# Cleanup: delete session, then project.
echo "--- Cleaning up ---"
curl -sf -X DELETE "$BASE_URL/api/sessions/$SESSION_ID" > /dev/null
echo "Deleted session $SESSION_ID"
curl -sf -X DELETE "$BASE_URL/api/projects/$PROJECT_ID" > /dev/null
echo "Deleted project $PROJECT_ID"

echo ""
echo "=== Done ==="
