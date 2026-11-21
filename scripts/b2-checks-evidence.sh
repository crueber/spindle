#!/bin/sh
# Evidence: the checks transition on a claim (pending + started_at) and on
# completion (success), with the runner's console lines.
set -eu
BIN=/var/tmp/b2-bin/spindle
DATA=/var/tmp/b2-evidence
PORT=18233
ADMIN=admin-secret
cd /var/tmp/wt-threads-b2
rm -rf "$DATA"; mkdir -p "$DATA"
cat > "$DATA/spindle.toml" <<EOF
[server]
listen = "0.0.0.0:$PORT"
auto_create_on_push = true
[server.auth]
mode = "token"
anonymous_read = true
tokens = [{ principal = "alice@example.com", token = "$ADMIN", write = true, admin = true }]
[store]
backend = "filesystem"
root = "$DATA/store"
[cache]
dir = "$DATA/cache"
EOF
SPINDLE_DATA_DIR="$DATA" "$BIN" > "$DATA/server.log" 2>&1 &
SPID=$!
cleanup() { kill "$SPID" 2>/dev/null || true; [ -f "$DATA/runner.pid" ] && kill "$(cat "$DATA/runner.pid")" 2>/dev/null || true; }
trap cleanup EXIT INT TERM
i=0; until curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; do i=$((i+1)); [ $i -gt 100 ] && exit 1; sleep 0.1; done
W=/tmp/b2-ev-work; rm -rf "$W"; mkdir -p "$W"; cd "$W"
git init -q -b main; git config user.email e@example.com; git config user.name E
mkdir -p .spindle/workflows
cat > .spindle/workflows/ci.yml <<'YAML'
name: CI
on: [push]
jobs:
  build:
    name: build
    runs-on: linux
    steps:
      - uses: actions/checkout
      - run: |
          echo "building $GITHUB_SHA"
          sleep 4
          echo "built"
YAML
git add -A; git commit -qm ci
git -c http.extraHeader="Authorization: Bearer $ADMIN" push -q "http://127.0.0.1:$PORT/alice/ev.git" main
SHA=$(git rev-parse HEAD)
TOK=$(curl -fsS -X POST -H "Authorization: Bearer $ADMIN" "http://127.0.0.1:$PORT/alice/ev/api/threads/runners/tokens" | sed 's/.*"token":"\([^"]*\)".*/\1/')
cd /var/tmp/wt-threads-b2
"$BIN" runner serve --server "http://127.0.0.1:$PORT" --register "$TOK" --token-file "$DATA/runner.token" \
  --labels linux --allow-host-docker --image alpine/git:latest --fetch-timeout 5 > "$DATA/runner.log" 2>&1 &
echo $! > "$DATA/runner.pid"
echo "--- checks while the job runs:"
i=0
while [ $i -lt 80 ]; do
  BODY=$(curl -s -H "Authorization: Bearer $ADMIN" "http://127.0.0.1:$PORT/alice/ev/api/checks/$SHA")
  case "$BODY" in *'"state":"pending"'*'"started_at"'*) echo "$BODY"; break;; esac
  i=$((i+1)); sleep 0.2
done
RUN=$(curl -s -H "Authorization: Bearer $ADMIN" "http://127.0.0.1:$PORT/alice/ev/api/threads?n=1" | sed 's/.*"id":"\([0-9-]*[a-z0-9]*\)".*/\1/')
echo "--- run while the job runs:"
curl -s -H "Authorization: Bearer $ADMIN" "http://127.0.0.1:$PORT/alice/ev/api/threads/$RUN" | head -c 1200; echo
i=0
while [ $i -lt 120 ]; do
  BODY=$(curl -s -H "Authorization: Bearer $ADMIN" "http://127.0.0.1:$PORT/alice/ev/api/checks/$SHA")
  case "$BODY" in *'"state":"success"'*) break;; esac
  i=$((i+1)); sleep 0.5
done
echo "--- checks after completion:"
echo "$BODY"
echo "--- job log (SSE frames via accept=json):"
curl -s -H "Authorization: Bearer $ADMIN" "http://127.0.0.1:$PORT/alice/ev/api/threads/$RUN/jobs/j-build/log?accept=json" | head -c 1500; echo
echo "--- runner console:"
cat "$DATA/runner.log"
