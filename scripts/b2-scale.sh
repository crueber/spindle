#!/bin/sh
# Pass B2 scale check (15 §6): many capacity-1 runners, one repo, a 20-cell
# matrix — every job runs exactly once and no container is left behind.
#
#   sh scripts/b2-scale.sh [RUNNERS] [JOBS_PER_RUNNER]
#
# Defaults: 20 runners, 20 jobs (a 5×4 matrix). Bounded: the whole script has a
# hard timeout and stops every process it started.
set -eu
ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN=${B2_BIN:-/var/tmp/b2-bin/spindle}
DATA=${B2_DATA:-/var/tmp/b2-scale}
PORT=${B2_PORT:-18111}
ADMIN=admin-secret
RUNNERS=${1:-20}
WANT=${2:-20}
START=$(date +%s)

cd "$ROOT"
[ -x "$BIN" ] || go build -o "$BIN" ./cmd/spindle
rm -rf "$DATA"; mkdir -p "$DATA"
cat > "$DATA/spindle.toml" <<EOF
[server]
listen = "0.0.0.0:$PORT"
auto_create_on_push = true
secrets_key = "0123456789abcdef0123456789abcdef"

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
cleanup() {
  for f in "$DATA"/runner.*.pid; do [ -f "$f" ] && kill "$(cat "$f")" 2>/dev/null || true; done
  [ -f "$DATA/server.pid" ] && kill "$(cat "$DATA/server.pid")" 2>/dev/null || true
  sleep 1
  for f in "$DATA"/runner.*.pid; do [ -f "$f" ] && kill -9 "$(cat "$f")" 2>/dev/null || true; done
  [ -f "$DATA/server.pid" ] && kill -9 "$(cat "$DATA/server.pid")" 2>/dev/null || true
}
trap 'cleanup' EXIT INT TERM

SPINDLE_DATA_DIR="$DATA" "$BIN" > "$DATA/server.log" 2>&1 &
echo $! > "$DATA/server.pid"
i=0
until curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; do
  i=$((i+1)); [ $i -gt 200 ] && { echo "server did not start"; tail -20 "$DATA/server.log"; exit 1; }
  sleep 0.1
done

W=/tmp/b2-scale-work; rm -rf "$W"; mkdir -p "$W"; cd "$W"
git init -q -b main; git config user.email scale@example.com; git config user.name Scale
mkdir -p .spindle/workflows
{
  echo 'name: Scale'
  echo 'on: [push]'
  echo 'jobs:'
  echo '  load:'
  echo '    name: load'
  echo '    runs-on: linux'
  echo '    strategy:'
  echo '      matrix:'
  echo '        a: [a1, a2, a3, a4, a5]'
  echo '        b: [b1, b2, b3, b4]'
  echo '    steps:'
  echo '      - uses: actions/checkout'
  echo '      - run: |'
  echo '          echo "cell $MATRIX_A/$MATRIX_B on $GITHUB_SHA"'
  echo '          sleep 1'
} > .spindle/workflows/load.yml
git add -A; git commit -qm scale
git -c http.extraHeader="Authorization: Bearer $ADMIN" push -q "http://127.0.0.1:$PORT/alice/scale.git" main

# One runner per job (capacity 1): the scale-out shape for untrusted code.
n=0
while [ $n -lt "$RUNNERS" ]; do
  TOK=$(curl -fsS -X POST -H "Authorization: Bearer $ADMIN" \
    "http://127.0.0.1:$PORT/alice/scale/api/threads/runners/tokens" \
    | sed 's/.*"token":"\([^"]*\)".*/\1/')
  ( SPINDLE_RUNNER_TOKEN="" "$BIN" runner serve --server "http://127.0.0.1:$PORT" \
      --register "$TOK" --token-file "$DATA/runner.$n.token" --labels linux \
      --allow-host-docker --image alpine/git:latest --fetch-timeout 5 \
      --name "scale-$n" >> "$DATA/runner.$n.log" 2>&1 & echo $! > "$DATA/runner.$n.pid" )
  n=$((n+1))
done

RUN=""
i=0
while [ -z "$RUN" ]; do
  i=$((i+1)); [ $i -gt 100 ] && { echo "no run"; exit 1; }
  RUN=$(curl -fsS -H "Authorization: Bearer $ADMIN" "http://127.0.0.1:$PORT/alice/scale/api/threads?n=1" \
    | sed 's/.*"id":"\([0-9-]*[a-z0-9]*\)".*/\1/')
  sleep 0.2
done
echo "run $RUN"

i=0
while :; do
  BODY=$(curl -fsS -H "Authorization: Bearer $ADMIN" "http://127.0.0.1:$PORT/alice/scale/api/threads/$RUN")
  STATE=$(echo "$BODY" | sed 's/.*"state":"\([a-z]*\)".*/\1/' | head -1)
  case "$STATE" in
    success|failure) break ;;
  esac
  i=$((i+1)); [ $i -gt 600 ] && { echo "run did not finish in 300s"; echo "$BODY" | head -c 2000; exit 1; }
  sleep 0.5
done
END=$(date +%s)
echo "run state: $STATE"

python3 - "$BODY" "$WANT" <<'PY'
import json,sys
data=json.loads(sys.argv[1]); want=int(sys.argv[2])
jobs=data["jobs"]
states=[j["state"] for j in jobs]
attempts=[j["attempt"] for j in jobs]
seqs=[j.get("queue_seq","") for j in jobs]
ids=[j["id"] for j in jobs]
steps=[len(j.get("step_results") or []) for j in jobs]
print(f"jobs={len(jobs)} want={want} success={states.count('success')} failure={states.count('failure')}")
print(f"unique job ids={len(set(ids))} unique queue seqs={len(set(seqs))} attempts={sorted(set(attempts))}")
print(f"jobs with 2 step results={steps.count(2)}")
ok = len(jobs)==want and states.count("success")==want and len(set(ids))==want and len(set(seqs))==want and set(attempts)=={1} and steps.count(2)==want
sys.exit(0 if ok else 1)
PY

echo "checks:"
curl -s -H "Authorization: Bearer $ADMIN" "http://127.0.0.1:$PORT/alice/scale/api/checks/$(git rev-parse HEAD)" \
  | sed 's/.*"total_counts":\({[^}]*}\).*/\1/'
echo "wall time: $((END-START))s"
sleep 2
LEFTOVER=$(docker ps -a --format '{{.Names}}' | grep -c '^sp-' || true)
echo "leftover sp- containers: $LEFTOVER"
[ "$LEFTOVER" = "0" ] || { docker ps -a --format '{{.Names}}' | grep '^sp-' | head; exit 1; }
echo OK
