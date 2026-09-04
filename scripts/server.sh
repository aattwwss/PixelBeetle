#!/usr/bin/env bash
# Run the game server (and optionally stop it). Keeps processes alive after
# this script exits via setsid.
#
# The server can be started EITHER via this script (bin/server) or directly
# as `go run ./cmd/server` (which compiles a temp binary under
# ~/.cache/go-build/...). Historically `stop` only matched bin/server, so a
# go-run server survived restarts and left a second instance trying to bind
# :8080. stop/start now deal in process PIDs and the port itself, not one
# cmdline shape.
set -u
cd "$(dirname "$0")/.."

TB="127.0.0.1:3000,127.0.0.1:3001,127.0.0.1:3002"

case "${1:-start}" in
  build)
    go build -o bin/server ./cmd/server && go build -o bin/bot ./cmd/bot
    ;;
  start)
    mkdir -p data/logs
    # Already up? Judge by the port, not by pgrep against our own cmdline.
    if ss -ltn 2>/dev/null | grep -qE '[:.]8080[[:space:]]'; then echo "server already running"; exit 0; fi
    "${0%/*}/tigerbeetle.sh" start >/dev/null
    go build -o bin/server ./cmd/server || exit 1
    setsid nohup ./bin/server -tb-addresses "$TB" -addr :8080 \
      -cdc-url "amqp://guest:guest@127.0.0.1:5672/" -cdc-exchange tigerbeetle \
      > data/logs/server.log 2>&1 &
    pid=$!
    # Verify THIS process really bound the port and answers healthz; a bind
    # failure (stale holder) or a crash shows up here instead of a lying "up".
    for _ in $(seq 1 20); do
      if kill -0 "$pid" 2>/dev/null && body=$(curl -sf http://localhost:8080/healthz 2>/dev/null); then
        echo "$body <- server up"
        exit 0
      fi
      sleep 0.25
    done
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "server failed to start — log tail:" >&2
      tail -5 data/logs/server.log >&2
    else
      echo "server process alive but /healthz not responding" >&2
    fi
    exit 1
    ;;
  stop)
    # Kill by PID list from ps. Matching args with awk's bracket regex can't
    # match our own cmdline the way pkill -f's pattern-in-argv can. Covers the
    # built binary (comm=server, running from bin/), the `go run` wrapper
    # (comm=go), and the cached temp binary go run execs (comm=server).
    pids=$(ps -eo pid=,comm=,args= | awk '
      ($2 == "server" || $2 == "go") &&
      ($0 ~ /[s]erver -tb-addresses/ || $0 ~ /[g]o run \.\/cmd\/server/) { print $1 }')
    [ -n "$pids" ] && kill $pids 2>/dev/null
    # Fallback: whatever unknown shape still holds :8080.
    command -v fuser >/dev/null && fuser -k 8080/tcp >/dev/null 2>&1
    # Wait for the port to actually free up (bounded), then for the old
    # process to fully exit: a dying server can close its listener while its
    # final anchor-sidecar evictions still run, and overlapping writers tear
    # permanent holes in the append-only sidecar (read as phantom changed
    # minutes by the timeline).
    for _ in $(seq 1 10); do
      if ! ss -ltn 2>/dev/null | grep -qE '[:.]8080[[:space:]]'; then break; fi
      sleep 0.5
    done
    for _ in $(seq 1 20); do
      remaining=$(ps -eo pid=,comm=,args= | awk '
        ($2 == "server" || $2 == "go") &&
        ($0 ~ /[s]erver -tb-addresses/ || $0 ~ /[g]o run \.\/cmd\/server/) { print $1 }')
      [ -z "$remaining" ] && break
      sleep 0.25
    done
    echo stopped
    ;;
  restart)
    "$0" stop; sleep 1; "$0" start
    ;;
  *)
    echo "usage: server.sh {build|start|stop|restart}"; exit 1
    ;;
esac