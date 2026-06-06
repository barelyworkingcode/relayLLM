#!/bin/bash
set -e
cd "$(dirname "$0")"

go build -o relayllm .
echo "Built relayllm binary."

(cd cmd/hook && go build -o hook .)
echo "Built hook binary."

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
codesign "${SIGN_ARGS[@]}" cmd/hook/hook
codesign --verify --strict --verbose=2 relayllm
codesign --verify --strict --verbose=2 cmd/hook/hook

/Applications/Relay.app/Contents/MacOS/relay service register \
  --name "Relay LLM" \
  --command "$(pwd)/relayllm" \
  --args "--router-port" \
  --args "8180" \
  --autostart \
  --no-frontend-creds   # backend: never dials relay's front door, so don't hand it the bearer (it would leak into spawned shells)
echo ""
echo "Registered with Relay."
