#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

/Applications/Relay.app/Contents/MacOS/relay service register \
  --name "oMLX" \
  --command "$(pwd)/run.oMLX.sh" \
  --url "http://127.0.0.1:8000/admin" \
  --autostart

echo ""
echo "Registered oMLX with Relay."
