#!/usr/bin/env bash
# Run the native TigerBeetle CDC job (tigerbeetle amqp). Single-instance by
# design; monitor and restart on crash. Keeps the process alive after this
# script exits via setsid.
set -u
cd "$(dirname "$0")/.."

TB="127.0.0.1:3000,127.0.0.1:3001,127.0.0.1:3002"
AMQP_ARGS="--host=127.0.0.1 --vhost=/ --user=guest --password=guest --publish-exchange=tigerbeetle"

case "${1:-start}" in
  start)
    mkdir -p data/logs
    if pgrep -f "tigerbeetle [a]mqp" >/dev/null; then echo "cdc already running"; exit 0; fi
    ./scripts/dev-rabbit.sh start >/dev/null || exit 1
    setsid nohup ./bin/tigerbeetle amqp \
      --addresses="$TB" --cluster=0 $AMQP_ARGS \
      > data/logs/cdc.log 2>&1 &
    sleep 2
    if pgrep -f "tigerbeetle [a]mqp" >/dev/null; then
      echo "cdc up (log: data/logs/cdc.log)"
    else
      echo "cdc failed to start:" >&2
      cat data/logs/cdc.log >&2
      exit 1
    fi
    ;;
  stop)
    pkill -f "tigerbeetle [a]mqp" && echo stopped || echo "not running"
    ;;
  status)
    pgrep -af "tigerbeetle [a]mqp" || echo "not running"
    ;;
  *)
    echo "usage: run-cdc.sh {start|stop|status}"; exit 1
    ;;
esac
