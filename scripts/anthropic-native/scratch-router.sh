#!/usr/bin/env bash
# Start or stop a scratch standalone relayLLM router for the native
# Anthropic pass-through checks.
#
# Usage: scratch-router.sh <worktree> [stop]
# Prints ROUTER_URL, ROUTER_KEY_FILE (0600, inside the temp dir), PID_FILE, LOG.
# Env:   SETTINGS_FILE  (required for start) settings.json to copy in
#        ROUTER_PORT    (default 18180)
#        STATE_FILE     (default ${TMPDIR:-/tmp}/relayllm-scratch-router.state)
set -euo pipefail

worktree="${1:?usage: scratch-router.sh <worktree> [stop]}"
action="${2:-start}"
STATE_FILE="${STATE_FILE:-${TMPDIR:-/tmp}/relayllm-scratch-router.state}"
ROUTER_PORT="${ROUTER_PORT:-18180}"

state_get() { sed -n "s/^$1=//p" "$STATE_FILE" 2>/dev/null | head -n1; }

stop_router() {
  [ -f "$STATE_FILE" ] || { echo "no state file at $STATE_FILE; nothing to stop"; return 0; }
  local tmp pidfile pid
  tmp="$(state_get TMP)"
  pidfile="$(state_get PID_FILE)"
  if [ -n "$pidfile" ] && [ -f "$pidfile" ]; then
    pid="$(cat "$pidfile")"
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      for _ in $(seq 1 50); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
      kill -9 "$pid" 2>/dev/null || true
    fi
  fi
  # Remove only a non-empty dir recorded by this script's own mktemp.
  if [ -n "$tmp" ] && [ -d "$tmp" ] && [ -n "$(ls -A "$tmp")" ] && [ -f "$tmp/.created-by-scratch-router" ]; then
    rm -rf "$tmp"
  else
    echo "not removing '$tmp': missing, empty or not created by this script" >&2
  fi
  rm -f "$STATE_FILE"
  echo "stopped"
}

if [ "$action" = "stop" ]; then stop_router; exit 0; fi

: "${SETTINGS_FILE:?SETTINGS_FILE is required}"
[ -f "$SETTINGS_FILE" ] || { echo "SETTINGS_FILE not found: $SETTINGS_FILE" >&2; exit 1; }
if [ -f "$STATE_FILE" ]; then echo "state file exists ($STATE_FILE); run '$0 $worktree stop' first" >&2; exit 1; fi

tmp="$(mktemp -d)"
touch "$tmp/.created-by-scratch-router"
pidfile="$tmp/relayllm.pid"
printf 'TMP=%s\nPID_FILE=%s\n' "$tmp" "$pidfile" > "$STATE_FILE"

(cd "$worktree" && go build -o "$tmp/relayllm" ./cmd/relayllm)
mkdir -p "$tmp/data"
cp "$SETTINGS_FILE" "$tmp/data/settings.json"

# A standalone router answers 401 everywhere until a router key exists.
keyfile="$tmp/router.key"
( umask 077; "$tmp/relayllm" router-key add --label scratch --data-dir "$tmp/data" 2>/dev/null > "$keyfile" )
chmod 600 "$keyfile"
[ -s "$keyfile" ] || { echo "router-key add produced no key" >&2; exit 1; }

unset RELAY_LAUNCH_FD
nohup "$tmp/relayllm" \
  --data-dir "$tmp/data" \
  --socket "$tmp/relayllm.sock" \
  --router-port "$ROUTER_PORT" \
  --router-bind 127.0.0.1 \
  > "$tmp/relayllm.log" 2>&1 &
echo $! > "$pidfile"

url="http://127.0.0.1:${ROUTER_PORT}"
for _ in $(seq 1 100); do
  if curl -fsS -o /dev/null -H "Authorization: Bearer $(cat "$keyfile")" "$url/health" 2>/dev/null; then
    echo "ROUTER_URL=$url"
    echo "ROUTER_KEY_FILE=$keyfile"
    echo "PID_FILE=$pidfile"
    echo "LOG=$tmp/relayllm.log"
    exit 0
  fi
  kill -0 "$(cat "$pidfile")" 2>/dev/null || { echo "relayllm exited early:" >&2; tail -n 20 "$tmp/relayllm.log" >&2; exit 1; }
  sleep 0.2
done
echo "timed out waiting for $url/health" >&2
tail -n 20 "$tmp/relayllm.log" >&2
exit 1
