#!/bin/bash
set -e
cd "$(dirname "$0")"

go build -o relayllm ./cmd/relayllm
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
  --args "--http-port" \
  --args "8181" \
  --args "--http-bind" \
  --args "127.0.0.1,192.168.64.1" \
  --url "http://localhost:8181/status" \
  --autostart \
  --capability manifest \
  --capability projects
  # --router-bind stays loopback-only (the flag's default): this devbox's
  # settings.json configures router.anthropic (real Anthropic API credential
  # passthrough), which refuses to start on a non-loopback --router-bind
  # without --router-tls-cert -- see relay-llm.log 2026-09-11 09:50 for the
  # actual refusal this produced when --router-bind briefly included
  # 192.168.64.1. Revisit if/when the router gets a TLS cert.
echo ""
echo "Registered with Relay."
