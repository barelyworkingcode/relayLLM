#!/bin/bash
set -e
cd "$(dirname "$0")"

go build -o relayllm .
echo "Built relayllm binary."

(cd cmd/hook && go build -o hook .)
echo "Built hook binary."

OPENAI_CONFIG_PATH="$HOME/Library/Application Support/relayLLM/openai_endpoints.json"

/Applications/Relay.app/Contents/MacOS/relay service register \
  --name "Relay LLM" \
  --command "$(pwd)/relayllm" \
  --args "--openai-config" \
  --args "$OPENAI_CONFIG_PATH" \
  --autostart
echo ""
echo "Registered with Relay."
