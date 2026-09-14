#!/bin/bash
set -e
cd "$(dirname "$0")"

go build -o relayllm ./cmd/relayllm
echo "Built relayllm binary."

# Code signing -- mirrors relay's build.sh so hardened-runtime + distribution
# parity stays consistent across every binary spawned by relay.
# RELAY_SIGN_IDENTITY lets you pin a specific cert when multiple are present.
IDENTITY="${RELAY_SIGN_IDENTITY:-$(security find-identity -v -p codesigning | grep "Developer ID Application" | grep -o '"[^"]*"' | head -1 | tr -d '"' || true)}"
if [ -n "$IDENTITY" ]; then
    echo "Signing with: $IDENTITY"
    SIGN_ARGS=(--force --sign "$IDENTITY" --options runtime --timestamp)
else
    echo "No Developer ID found, ad-hoc signing"
    SIGN_ARGS=(--force --sign - --options runtime)
fi
codesign "${SIGN_ARGS[@]}" relayllm
codesign --verify --strict --verbose=2 relayllm

# --router-port is dropped from this registration: C9 refuses to start with
# one configured while relay launched the process — relay reaches model
# routing only through router.sock, registered at runtime via
# RegisterModelHost, not through a listed port here.
/Applications/Relay.app/Contents/MacOS/relay service register \
  --name "Relay LLM" \
  --command "$(pwd)/relayllm" \
  --args "--http-port" \
  --args "8181" \
  --args "--http-bind" \
  --args "127.0.0.1,192.168.64.1" \
  --args "--router-bind" \
  --args "127.0.0.1,192.168.64.1" \
  --url "http://localhost:8181/status" \
  --autostart \
  --capability manifest \
  --capability model_host
  # model_host (C9, plan-broker-and-sessions.md §2) is what lets relay accept
  # this service's RegisterModelHost call for router.sock; a service record
  # missing it makes relay refuse the registration and relayLLM exit 78.
  # "projects" is deliberately absent: relay-sessions, not relayLLM, hosts
  # sessions and projects, and relayLLM's own registration must never ask
  # for a grant relay does not issue for this service.
echo ""
echo "Registered with Relay."
