#!/usr/bin/env bash
# End-to-end check for the FlowHub YouTrack webhook receiver.
#
# Starts a real process on a loopback port, posts one delivery per interesting
# shape, then asserts the HTTP responses, the audit trail, the log files and the
# promise that no credential reaches disk. Everything it creates lives in a
# temporary directory that is removed on exit.
#
# Usage: make smoke            (or: BIN=./bin/flowhub ./scripts/smoke.sh)
set -uo pipefail

BIN="${BIN:-./bin/flowhub}"
PORT="${SMOKE_PORT:-18080}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/flowhub-smoke.XXXXXX")"
DATA="$WORK/data"
LOG="$WORK/server.log"
PID=""

cleanup() {
  if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
    kill -TERM "$PID" 2>/dev/null
    wait "$PID" 2>/dev/null
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  if [ -f "$LOG" ]; then
    echo "--- server log ---" >&2
    cat "$LOG" >&2
  fi
  exit 1
}

# Octal permission bits. The implementation is detected explicitly because a Nix
# shell puts GNU coreutils' stat ahead of the BSD one, where `-f` means
# "filesystem" and produces the wrong output with a zero exit status.
file_mode() { # path
  if stat --version >/dev/null 2>&1; then
    stat -c '%a' "$1"
  else
    stat -f '%Lp' "$1"
  fi
}

[ -x "$BIN" ] || fail "$BIN not found; run 'make build' first"
command -v jq >/dev/null || fail "jq is required"

KEY="$(openssl rand -hex 32)"
TOKEN="$(openssl rand -hex 32)"
BASE="http://127.0.0.1:$PORT"
URL="$BASE/hooks/youtrack/$KEY"

echo "== start =="
# A test must not read the operator's own environment file. This run asserts its own
# key, token and data directory, so a ~/.config/flowhub/.env with other values — or
# merely a permissive mode, which is refused — would make the result depend on the
# machine it ran on. `FLOWHUB_ENV_FILE=-` disables the file (ADR 0005).
FLOWHUB_ENV_FILE=- \
FLOWHUB_ADDR="127.0.0.1:$PORT" \
FLOWHUB_HOOK_KEY="$KEY" \
FLOWHUB_TOKEN="$TOKEN" \
FLOWHUB_ALLOWED_SOURCES=127.0.0.1 \
FLOWHUB_DATA_DIR="$DATA" \
FLOWHUB_LOG_HEADERS=true \
  "$BIN" >"$LOG" 2>&1 &
PID=$!

for _ in $(seq 1 100); do
  curl -sf "$BASE/healthz" >/dev/null 2>&1 && break
  kill -0 "$PID" 2>/dev/null || fail "receiver exited during startup"
  sleep 0.1
done
curl -sf "$BASE/healthz" >/dev/null || fail "receiver did not become healthy on $BASE"
echo "  listening on $BASE (data dir $DATA)"

TS="$(date -u +%Y-%m-%dT%H:%M:%S.000Z)"
ISSUE='{"event":"issueUpdated","timestamp":"'"$TS"'","id":"2-123","summary":"Fix login","project":{"key":"SP","name":"Sample Project","shortName":"SP"},"updatedBy":{"login":"jane.doe","fullName":"Jane Doe","email":"jane@example.com"},"changedFields":[{"name":"State","oldValue":{"name":"Open","presentation":"Open"},"value":{"name":"In Progress","presentation":"In Progress"}}]}'

post() { # token body -> "code time"
  curl -s -o /dev/null -w '%{http_code} %{time_total}' -X POST "$URL" \
    -H 'Content-Type: application/json' -H "X-YouTrack-Token: $1" --data-binary "$2"
}

check_code() { # label expected actual
  local got="${3%% *}"
  [ "$got" = "$2" ] || fail "$1: HTTP $got, want $2"
  printf '  %-18s -> %s\n' "$1" "$3"
}

echo "== deliveries =="
check_code "accepted"        202 "$(post "$TOKEN" "$ISSUE")"
check_code "duplicate"       202 "$(post "$TOKEN" "$ISSUE")"
check_code "bad token"       202 "$(post "wrong-token" "$ISSUE")"
check_code "literal secret"  202 "$(post "secret" "$ISSUE")"
check_code "bad url key"     202 "$(curl -s -o /dev/null -w '%{http_code} %{time_total}' \
  -X POST "$BASE/hooks/youtrack/deadbeef" -H 'Content-Type: application/json' \
  -H "X-YouTrack-Token: $TOKEN" --data-binary "$ISSUE")"
check_code "unknown event"   202 "$(post "$TOKEN" '{"event":"issueRenamed","timestamp":"'"$TS"'","id":"2-9"}')"
check_code "invalid json"    202 "$(post "$TOKEN" '{"oops"')"
check_code "stale timestamp" 202 "$(post "$TOKEN" '{"event":"issueCreated","timestamp":"2020-01-01T00:00:00.000Z","id":"2-8"}')"
check_code "GET"             405 "$(curl -s -o /dev/null -w '%{http_code} %{time_total}' "$URL")"

echo "== audit trail =="
sleep 0.3
JSONL=$(ls "$DATA"/webhook-*.jsonl 2>/dev/null | head -1)
[ -n "$JSONL" ] && [ -f "$JSONL" ] || fail "no JSONL audit file was written"

accepted="$(jq -r 'select(.accepted) | .event' "$JSONL" | grep -c .)"
[ "$accepted" = "1" ] || fail "want exactly one accepted delivery, got $accepted"
printf '  %-22s %s\n' "accepted" "$accepted"

for pair in "duplicate:1" "bad_header_token:2" "bad_url_key:1" "unknown_event:1" \
            "invalid_json:1" "outside_replay_window:1" "method_not_allowed:1"; do
  reason="${pair%%:*}"
  want="${pair##*:}"
  got="$(jq -r --arg r "$reason" 'select(.reason == $r) | .reason' "$JSONL" | grep -c .)"
  [ "$got" = "$want" ] || fail "rejection $reason: got $got, want $want"
  printf '  %-22s %s\n' "$reason" "$got"
done

echo "== payload analysis =="
jq -e 'select(.accepted) | (.payload_schema | length) > 0' "$JSONL" >/dev/null \
  || fail "accepted record carries no payload schema"
jq -e 'select(.accepted) | .has_number_in_project == false' "$JSONL" >/dev/null \
  || fail "numberInProject probe is missing"
jq -e 'select(.accepted) | (.headers["X-Youtrack-Token"][0] | startswith("<masked"))' "$JSONL" >/dev/null \
  || fail "token header was not masked"
jq -e 'select(.token_header.matches_configured) | .token_header.matches_configured' "$JSONL" >/dev/null \
  || fail "token fingerprint does not match the configured token"
jq -e 'select(.token_header.looks_like_literal_secret) | .token_header.value_len == 6' "$JSONL" >/dev/null \
  || fail 'the literal "secret" delivery was not flagged'
echo "  schema paths, numberInProject probe, token fingerprint: ok"

echo "== log files =="
PAYLOAD_LOG=$(ls "$DATA"/payload-*.log 2>/dev/null | head -1)
for f in "$DATA/flowhub.log" "$PAYLOAD_LOG"; do
  [ -n "$f" ] && [ -f "$f" ] || fail "expected log file is missing"
  perm="$(file_mode "$f")"
  [ "$perm" = "600" ] || fail "$f has mode $perm, want 600"
  printf '  %-30s %8s bytes, mode %s\n' "$(basename "$f")" "$(wc -c <"$f" | tr -d ' ')" "$perm"
done
grep -q '^schema' "$PAYLOAD_LOG" || fail "payload log has no schema section"
grep -q 'literal_secret=true' "$PAYLOAD_LOG" || fail "payload log does not show the literal-secret finding"
grep -q 'matches_configured=true' "$PAYLOAD_LOG" || fail "payload log does not show the token comparison"

echo "== credential containment =="
if grep -rq "$TOKEN" "$DATA"; then fail "the raw token appears under $DATA"; fi
if grep -rq "$KEY" "$DATA"; then fail "the URL key appears under $DATA"; fi
echo "  neither the token nor the URL key was written to disk"

echo "== shutdown =="
kill -TERM "$PID"
wait "$PID" 2>/dev/null
PID=""
grep -q 'persisted=' "$DATA/flowhub.log" || fail "no drain summary in the application log"
tail -1 "$DATA/flowhub.log"

echo
echo "SMOKE PASSED"
