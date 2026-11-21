#!/bin/sh
# Manual pass-B2 e2e driver (15_threads.md §4.1): a real server, a real runner
# and real docker containers, from one push to green checks. Bounded by a
# timeout; the trap stops the server and the runner. Automated equivalent:
# internal/e2e/threads_runner_test.go.
set -eu
ROOT=/var/tmp/wt-threads-b2
BIN=/var/tmp/b2-bin/spindle
DATA=/var/tmp/b2-data
PORT=18099
ADMIN=admin-secret
mkdir -p "$DATA" "$(dirname "$BIN")"
cd "$ROOT"
go build -o "$BIN" ./cmd/spindle
rm -rf "$DATA"; mkdir -p "$DATA"
cat > "$DATA/spindle.toml" <<EOF
[server]
listen = "0.0.0.0:$PORT"
auto_create_on_push = true
secrets_key = "0123456789abcdef0123456789abcdef"

[server.auth]
mode = "token"
anonymous_read = true
tokens = [
  { principal = "alice@example.com", token = "$ADMIN", write = true, admin = true },
]

[store]
backend = "filesystem"
root = "$DATA/store"

[cache]
dir = "$DATA/cache"
EOF
SPINDLE_DATA_DIR="$DATA" "$BIN" > "$DATA/server.log" 2>&1 &
SPID=$!
echo $SPID > "$DATA/server.pid"
trap 'kill $SPID 2>/dev/null; kill $(cat "$DATA/runner.pid" 2>/dev/null) 2>/dev/null' EXIT
i=0
until curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; do
  i=$((i+1)); [ $i -gt 100 ] && { echo "server did not start"; cat "$DATA/server.log"; exit 1; }
  sleep 0.1
done
echo "server up on $PORT"
W=/tmp/b2-work; rm -rf "$W"; mkdir -p "$W"
cd "$W"
git init -q -b main
git config user.email e2e@example.com
git config user.name E2E
mkdir -p .spindle/workflows
cat > .spindle/workflows/ci.yml <<'YAML'
name: CI
on: [push]
jobs:
  lint:
    name: lint
    runs-on: linux
    steps:
      - uses: actions/checkout
      - name: lint
        run: |
          echo "linting $GITHUB_SHA"
          test -f .spindle/workflows/ci.yml && echo "workspace ok"
  test:
    name: test
    runs-on: linux
    needs: [lint]
    steps:
      - uses: actions/checkout
      - run: echo "tests pass"
YAML
git add -A && git commit -qm "ci"
git -c http.extraHeader="Authorization: Bearer $ADMIN" push -q "http://127.0.0.1:$PORT/alice/demo.git" main
echo "pushed"
# mint a runner registration token
TOK=$(curl -fsS -X POST -H "Authorization: Bearer $ADMIN" "http://127.0.0.1:$PORT/alice/demo/api/threads/runners/tokens" | sed 's/.*"token":"\([^"]*\)".*/\1/')
echo "registration token: ${TOK%%.*}…"
cd "$ROOT"
SPINDLE_RUNNER_TOKEN= "$BIN" runner serve --server "http://127.0.0.1:$PORT" \
  --register "$TOK" --token-file "$DATA/runner.token" --labels linux \
  --allow-host-docker --image alpine/git:latest --fetch-timeout 5 \
  > "$DATA/runner.log" 2>&1 &
echo $! > "$DATA/runner.pid"
i=0
until curl -fsS -H "Authorization: Bearer $ADMIN" "http://127.0.0.1:$PORT/alice/demo/api/threads" | grep -q '"state":"success"'; do
  i=$((i+1)); [ $i -gt 120 ] && { echo "run did not finish"; cat "$DATA/runner.log"; curl -s -H "Authorization: Bearer $ADMIN" "http://127.0.0.1:$PORT/alice/demo/api/threads"; exit 1; }
  sleep 1
done
echo "run success"
curl -s -H "Authorization: Bearer $ADMIN" "http://127.0.0.1:$PORT/alice/demo/api/threads" | head -c 1200; echo
RUN=$(curl -s -H "Authorization: Bearer $ADMIN" "http://127.0.0.1:$PORT/alice/demo/api/threads" | sed 's/.*"id":"\([0-9-]*[a-z0-9]*\)".*/\1/' | head -1)
echo "RUN=$RUN"
curl -s -H "Authorization: Bearer $ADMIN" "http://127.0.0.1:$PORT/alice/demo/api/threads/$RUN" | head -c 4000; echo
echo "--- checks"
curl -s -H "Authorization: Bearer $ADMIN" "http://127.0.0.1:$PORT/alice/demo/api/checks/$(cd $W && git rev-parse HEAD)" | head -c 2000; echo
echo "--- runner log"
tail -20 "$DATA/runner.log"
