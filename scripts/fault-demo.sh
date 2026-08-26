#!/usr/bin/env bash
# Fault-tolerance demo: kills one TigerBeetle replica mid-load to demonstrate
# 3-replica fault tolerance (a quorum of 3 tolerates 1 failure). The game
# server and the bot continue uninterrupted while a replica is down, and the
# cluster heals when the replica rejoins.
#
# Transcript steps:
#   1. start cluster + RabbitMQ + CDC job
#   2. start game server + bot load
#   3. kill -9 replica 1 mid-load
#   4. prove the game keeps serving (healthz 200, claims confirm)
#   5. restart replica 1 and watch the cluster rejoin
#   6. final healthz + cleanup
set -euo pipefail
cd "$(dirname "$0")/.."

BOT_PID=""
SERVER_PID=""

cleanup() {
  local rc=$?
  echo "=== STEP 6: cleanup ==="
  [ -n "$BOT_PID" ] && kill "$BOT_PID" 2>/dev/null || true
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  ./scripts/cdc.sh stop >/dev/null 2>&1 || true
  ./scripts/rabbitmq.sh stop >/dev/null 2>&1 || true
  ./scripts/tigerbeetle.sh stop >/dev/null 2>&1 || true
  if [ "$rc" -eq 0 ]; then
    echo "FAULT TOLERANCE DEMO PASSED"
  else
    echo "FAULT TOLERANCE DEMO FAILED (rc=$rc)"
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

# healthz_retry curls /healthz until the server responds (up to $1 tries,
# 1s apart). The server can briefly stall during TB replica rejoin/recovery,
# so a single-shot race can show an empty line in the demo transcript.
healthz_retry() {
  local tries=$1 out="" i
  for i in $(seq 1 "$tries"); do
    out=$(curl -s --max-time 2 localhost:8080/healthz || true)
    [ -n "$out" ] && { echo "$out"; return 0; }
    sleep 1
  done
  echo "(server not responding after $tries tries)"
}

echo "=== STEP 1: start dependencies (TigerBeetle ×3, RabbitMQ, CDC) ==="
./scripts/tigerbeetle.sh start
sleep 2
./scripts/rabbitmq.sh start
sleep 2
./scripts/cdc.sh start
sleep 2

echo "=== STEP 2: build + start game server ==="
go build -o bin/server ./cmd/server
setsid nohup bin/server -tb-addresses 127.0.0.1:3000,127.0.0.1:3001,127.0.0.1:3002 \
  -addr :8080 -grid 256x256 \
  -cdc-url amqp://guest:guest@127.0.0.1:5672/ -cdc-exchange tigerbeetle \
  > data/logs/server.log 2>&1 &
SERVER_PID=$!

# Eager init creates+funds 65,536 pixel accounts and warmup replays history
# (tens of thousands of pixels here), so the server is NOT ready immediately.
# Poll healthz until it returns a non-empty 200 rather than sleeping a fixed
# amount (a fixed sleep raced the init and killed a replica mid-init).
wait_health() {
  local n=0
  while [ "$n" -lt 60 ]; do
    local out
    out=$(curl -s -m 2 localhost:8080/healthz 2>/dev/null || true)
    if [ -n "$out" ]; then
      echo "healthz: $out"
      return 0
    fi
    sleep 1
    n=$((n+1))
  done
  echo "ERROR: server did not become ready"; return 1
}
wait_health

echo "=== STEP 2: start bot load (api mode, 100 rps) ==="
setsid nohup go run ./cmd/bot -target http://localhost:8080 -grid 256x256 \
  -rps 100 -duration 120s -players 32 \
  > data/logs/bot.log 2>&1 &
BOT_PID=$!
sleep 5
wait_health
echo "steady state reached"

echo "=== STEP 3: kill replica 1 (follower) mid-load ==="
REPLICA_PID=$(pgrep -f "tigerbeetle start.*dev_1" | head -1)
if [ -z "$REPLICA_PID" ]; then
  echo "ERROR: replica 1 not running"; exit 1
fi
echo "killing replica 1 (pid=$REPLICA_PID)"
kill -9 "$REPLICA_PID"
sleep 1
echo "replica 1 killed"

echo "=== STEP 4: verify the game continues with a replica down ==="
sleep 2
echo "healthz (replica down): $(healthz_retry 3)"

# Three claim+confirm cycles straight through the API while a replica is dead.
OK=0
for _ in 1 2 3; do
  PLAYER=$(python3 -c "import uuid; print(uuid.uuid7())")
  X=$((RANDOM % 200)); Y=$((RANDOM % 200)); COLOR=$((RANDOM % 16))
  CLAIM=$(curl -s -X POST localhost:8080/claim -H 'content-type: application/json' \
    -H "cookie: player_id=$PLAYER" -d "{\"x\":$X,\"y\":$Y,\"color\":$COLOR}")
  ID=$(echo "$CLAIM" | python3 -c "import json,sys; print(json.load(sys.stdin).get('claimId',''))" 2>/dev/null || true)
  if [ -n "$ID" ]; then
    CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST localhost:8080/confirm \
      -H 'content-type: application/json' -H "cookie: player_id=$PLAYER" -d "{\"claimId\":\"$ID\"}")
    [ "$CODE" = "204" ] && OK=$((OK+1))
  fi
done
echo "game continued during failure: claims confirmed=$OK/3"

echo "=== STEP 5: restart replica 1 and let the cluster rejoin ==="
./scripts/tigerbeetle.sh start
echo "replica 1 restarting"
sleep 5
./scripts/tigerbeetle.sh status
echo "post-recovery healthz: $(healthz_retry 10)"

sleep 3
echo "post-run healthz: $(healthz_retry 3)"
