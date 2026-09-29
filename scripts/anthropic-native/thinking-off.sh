#!/usr/bin/env bash
# Criterion 5: does an engine honour native thinking-off through the router?
#
# Needs ROUTER_KEY or ROUTER_KEY_FILE (sent as X-Relay-Router-Key, never printed).
# Usage: ROUTER_URL=http://127.0.0.1:18180 MODEL_KEYS="key1 key2" thinking-off.sh
# Output per key:
#   key=<k> disabled_thinking_blocks=N disabled_reasoning_text=N enabled_thinking_blocks=N PASS|FAIL
set -euo pipefail
: "${ROUTER_URL:?ROUTER_URL is required}"
: "${MODEL_KEYS:?MODEL_KEYS is required (space-separated)}"

if [ -z "${ROUTER_KEY:-}" ]; then
  : "${ROUTER_KEY_FILE:?ROUTER_KEY or ROUTER_KEY_FILE is required}"
  ROUTER_KEY="$(cat "$ROUTER_KEY_FILE")"
fi
export ROUTER_KEY

analyse='
import json, re, sys
d = json.load(sys.stdin)
blocks = d.get("content") or []
think = sum(1 for b in blocks if b.get("type") in ("thinking", "redacted_thinking"))
pat = re.compile(r"<think>|thinking process", re.I)
text = sum(len(pat.findall(b.get("text", ""))) for b in blocks if b.get("type") == "text")
print(think, text)
'

call() { # key thinking-json
  python3 - "$1" "$2" <<'PY' | curl -sS --max-time 300 "${ROUTER_URL}/v1/messages" \
      -H 'content-type: application/json' -H 'anthropic-version: 2023-06-01' \
      -H 'x-api-key: dummy' -H @<(printf 'X-Relay-Router-Key: %s\n' "$ROUTER_KEY") --data-binary @- | python3 -c "$analyse"
import json, sys
print(json.dumps({
  "model": sys.argv[1],
  "max_tokens": 400,
  "stream": False,
  "thinking": json.loads(sys.argv[2]),
  "system": [{"type": "text", "text": "You are a concise assistant."}],
  "messages": [{"role": "user", "content": [{"type": "text", "text": "What is 17 * 23? Answer with the number."}]}],
}))
PY
}

rc=0
for k in $MODEL_KEYS; do
  read -r d_think d_text < <(call "$k" '{"type":"disabled"}')
  read -r e_think _ < <(call "$k" '{"type":"enabled","budget_tokens":1024}')
  if [ "$d_think" -eq 0 ] && [ "$d_text" -eq 0 ]; then res=PASS; else res=FAIL; rc=1; fi
  echo "key=$k disabled_thinking_blocks=$d_think disabled_reasoning_text=$d_text enabled_thinking_blocks=$e_think $res"
done
exit $rc
