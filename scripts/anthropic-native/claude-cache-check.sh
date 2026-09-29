#!/usr/bin/env bash
# Criterion 8: real Claude Code against the router, two turns via --continue.
#
# Usage: ROUTER_URL=... MODEL_KEY=... EXPECT=zero|reuse [OMLX_LOG_CMD=...] claude-cache-check.sh
#   EXPECT=reuse: PASS when cache_read / (input + cache_read + cache_creation) > 0.80
#   EXPECT=zero:  PASS when cache_read == 0 on the second call
set -euo pipefail
: "${ROUTER_URL:?ROUTER_URL is required}"
: "${MODEL_KEY:?MODEL_KEY is required}"
: "${EXPECT:?EXPECT is required (zero|reuse)}"
case "$EXPECT" in zero|reuse) ;; *) echo "EXPECT must be zero or reuse" >&2; exit 2;; esac

scratch="$(mktemp -d)"
touch "$scratch/.created-by-claude-cache-check"
trap 'if [ -d "$scratch" ] && [ -n "$(ls -A "$scratch")" ]; then rm -rf "$scratch"; fi' EXIT

run_claude() { # extra args...
  (cd "$scratch" && ANTHROPIC_BASE_URL="$ROUTER_URL" ANTHROPIC_API_KEY=dummy \
    claude -p "$PROMPT" --model "$MODEL_KEY" --output-format json "$@")
}

PROMPT="List three prime numbers greater than 100, one per line."
run_claude > "$scratch/first.json"
PROMPT="Now list three more, different from before."
run_claude --continue > "$scratch/second.json"

rc=0
python3 - "$scratch/first.json" "$scratch/second.json" "$EXPECT" <<'PY' || rc=$?
import json, sys
first, second, expect = sys.argv[1:4]
u1 = json.load(open(first)).get("usage", {})
u2 = json.load(open(second)).get("usage", {})
print("call1 usage:", json.dumps(u1, sort_keys=True))
print("call2 usage:", json.dumps(u2, sort_keys=True))
inp = u2.get("input_tokens", 0); cr = u2.get("cache_read_input_tokens", 0); cc = u2.get("cache_creation_input_tokens", 0)
denom = inp + cr + cc
ratio = cr / denom if denom else 0.0
ok = (ratio > 0.80) if expect == "reuse" else (cr == 0)
print(f"expect={expect} cache_read={cr} ratio={ratio:.3f} {'PASS' if ok else 'FAIL'}")
sys.exit(0 if ok else 1)
PY
if [ -n "${OMLX_LOG_CMD:-}" ]; then
  echo "log_cached=$(bash -c "$OMLX_LOG_CMD" 2>&1 | /usr/bin/grep -o 'cached=[0-9]*' | tail -n1 | cut -d= -f2 || true)"
fi
exit $rc
