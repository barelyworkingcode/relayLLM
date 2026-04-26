#!/bin/bash
set -e
cd "$(dirname "$0")"

go build -o relayllm .
echo "Built relayllm binary."

(cd cmd/hook && go build -o hook .)
echo "Built hook binary."


/Applications/Relay.app/Contents/MacOS/relay service register \
  --name "Relay LLM" \
  --command "$(pwd)/relayllm" \
  --args "--comfyui-url" \
  --args "http://localhost:8188" \
  --autostart
echo ""
echo "Registered with Relay."
