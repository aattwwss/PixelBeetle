#!/usr/bin/env bash
# Native 3-replica TigerBeetle dev cluster (docs recommend running TB directly,
# not in Docker). Logs under data/logs/, data files under data/.
set -u
cd "$(dirname "$0")/.."

REPLICA_COUNT=3
BASE_PORT=3000
ADDRESSES=""
for i in $(seq 0 $((REPLICA_COUNT-1))); do
  ADDRESSES+="127.0.0.1:$((BASE_PORT+i)),"
done
ADDRESSES="${ADDRESSES%,}"

case "${1:-start}" in
  format)
    for i in $(seq 0 $((REPLICA_COUNT-1))); do
      f="data/dev_$i.tigerbeetle"
      test -f "$f" || ./bin/tigerbeetle format --cluster=0 --replica=$i --replica-count=$REPLICA_COUNT "$f"
    done
    ;;
  start)
    mkdir -p data/logs
    ./scripts/dev-cluster.sh format >/dev/null
    for i in $(seq 0 $((REPLICA_COUNT-1))); do
      if pgrep -f "tigerbeetle start.*dev_$i" >/dev/null; then
        echo "replica$i already running"; continue
      fi
      setsid nohup ./bin/tigerbeetle start --addresses="$ADDRESSES" "data/dev_$i.tigerbeetle" \
        > data/logs/replica$i.log 2>&1 &
      echo "replica$i -> :$((BASE_PORT+i)) pid=$!"
    done
    ;;
  stop)
    pkill -f "tigerbeetle start" && echo "stopped" || echo "nothing to stop"
    ;;
  status)
    pgrep -fa "tigerbeetle start" | sed 's/--addresses=[^ ]*//' || echo "not running"
    ;;
  *)
    echo "usage: dev-cluster.sh {format|start|stop|status}"; exit 1
    ;;
esac
