#!/usr/bin/env bash
# Run the game server (and optionally stop it). Keeps processes alive after
# this script exits via setsid.
set -u
cd "$(dirname "$0")/.."

TB="127.0.0.1:3000,127.0.0.1:3001,127.0.0.1:3002"

case "${1:-start}" in
  build)
    go build -o bin/server ./cmd/server && go build -o bin/bot ./cmd/bot
    ;;
  start)
    mkdir -p data/logs
    if pgrep -f "bin/server " >/dev/null; then echo "server already running"; exit 0; fi
    "${0%/*}/dev-cluster.sh" start >/dev/null
    go build -o bin/server ./cmd/server || exit 1
    setsid nohup ./bin/server -tb-addresses "$TB" -addr :8080 > data/logs/server.log 2>&1 &
    sleep 2
    curl -s http://localhost:8080/healthz && echo " <- server up"
    ;;
  stop)
    pkill -f "exe/server"; pkill -f "bin/server "; echo stopped
    ;;
  restart)
    "$0" stop; sleep 1; "$0" start
    ;;
  *)
    echo "usage: run-server.sh {build|start|stop|restart}"; exit 1
    ;;
esac
