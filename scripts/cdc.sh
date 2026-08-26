#!/usr/bin/env bash
# Run the native TigerBeetle CDC job (tigerbeetle amqp) under a restart
# supervisor loop. Single-instance by design. A supervisor marker
# (# pb-cdc-supervisor) makes start/stop matching precise.
set -u
cd "$(dirname "$0")/.."

TB="127.0.0.1:3000,127.0.0.1:3001,127.0.0.1:3002"
AMQP_ARGS="--host=127.0.0.1 --vhost=/ --user=guest --password=guest --publish-exchange=tigerbeetle"

SUPERVISOR='# pb-cdc-supervisor
while true; do
  ./bin/tigerbeetle amqp \
    --addresses="'"$TB"'" --cluster=0 '"$AMQP_ARGS"' \
    >> data/logs/cdc.log 2>&1
  code=$?
  echo "$(date -u +%FT%TZ) cdc exited ($code) — restarting in 2s" >> data/logs/cdc.log
  sleep 2
done'

case "${1:-start}" in
  start)
    mkdir -p data/logs
    if pgrep -f "pb-cdc-supervisor" >/dev/null; then echo "cdc already running"; exit 0; fi
    ./scripts/rabbitmq.sh start >/dev/null || exit 1
    setsid nohup bash -c "$SUPERVISOR" >/dev/null 2>&1 &
    sleep 3
    if pgrep -f "pb-cdc-supervisor" >/dev/null; then
      echo "cdc up (supervised; log: data/logs/cdc.log)"
    else
      echo "cdc failed to start:" >&2
      cat data/logs/cdc.log >&2
      exit 1
    fi
    ;;
  stop)
    # Kill the supervisor FIRST so it cannot resurrect the job.
    pkill -f "pb-cdc-supervisor"
    sleep 1
    pkill -f "bin/tigerbeetle amqp"
    echo stopped
    ;;
  status)
    pgrep -af "pb-cdc-supervisor|bin/tigerbeetle amqp" || echo "not running"
    ;;
  *)
    echo "usage: cdc.sh {start|stop|status}"; exit 1
    ;;
esac
