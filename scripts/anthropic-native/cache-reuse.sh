#!/usr/bin/env bash
# Criterion 7: does turn 2 reuse the prompt cache built by turn 1?
#
# Usage: ROUTER_URL=... MODEL_KEY=... [OMLX_LOG_CMD='cmd printing engine log tail'] cache-reuse.sh
# PASS when turn2_cache_read / turn1_total_prompt >= 0.90.
set -euo pipefail
: "${ROUTER_URL:?ROUTER_URL is required}"
: "${MODEL_KEY:?MODEL_KEY is required}"

work="$(mktemp -d)"
trap 'if [ -n "$(ls -A "$work" 2>/dev/null)" ]; then rm -rf "$work"; fi' EXIT

python3 - "$MODEL_KEY" "$ROUTER_URL" "$work" <<'PY'
import json, sys, urllib.request
model, url, work = sys.argv[1:4]

# Deterministic filler, roughly 7k tokens.
filler = " ".join(f"Record {i}: the quick brown fox number {i} jumps over lazy dog {i * 7 % 101}." for i in range(700))
system = [{"type": "text", "text": "You are a concise assistant. Reference material follows.\n" + filler,
           "cache_control": {"type": "ephemeral"}}]

def post(body):
    req = urllib.request.Request(url + "/v1/messages", data=json.dumps(body).encode(),
        headers={"content-type": "application/json", "anthropic-version": "2023-06-01", "x-api-key": "dummy"})
    with urllib.request.urlopen(req, timeout=600) as r:
        return json.load(r)

def total(u):
    return u.get("input_tokens", 0) + u.get("cache_read_input_tokens", 0) + u.get("cache_creation_input_tokens", 0)

msgs = [{"role": "user", "content": [{"type": "text", "text": "Say hello in five words."}]}]
r1 = post({"model": model, "max_tokens": 200, "stream": False, "system": system, "messages": msgs})
u1 = r1.get("usage", {})
reply = [b for b in r1.get("content", []) if b.get("type") == "text"] or [{"type": "text", "text": "hello"}]
msgs2 = msgs + [{"role": "assistant", "content": reply},
                {"role": "user", "content": [{"type": "text", "text": "Now say goodbye in five words."}]}]
r2 = post({"model": model, "max_tokens": 200, "stream": False, "system": system, "messages": msgs2})
u2 = r2.get("usage", {})
t1 = total(u1)
ratio = (u2.get("cache_read_input_tokens", 0) / t1) if t1 else 0.0
open(work + "/result", "w").write(
    f"turn1_input={t1} turn2_input_tokens={u2.get('input_tokens', 0)} "
    f"turn2_cache_read={u2.get('cache_read_input_tokens', 0)} ratio={ratio:.3f}\n"
    + ("PASS\n" if ratio >= 0.90 else "FAIL\n"))
PY

line="$(sed -n 1p "$work/result")"
verdict="$(sed -n 2p "$work/result")"
log_cached=""
if [ -n "${OMLX_LOG_CMD:-}" ]; then
  log_cached="$(bash -c "$OMLX_LOG_CMD" 2>&1 | /usr/bin/grep -o 'cached=[0-9]*' | tail -n1 | cut -d= -f2 || true)"
  echo "$line log_cached=${log_cached:-n/a} $verdict"
else
  echo "$line $verdict"
fi
[ "$verdict" = PASS ]
