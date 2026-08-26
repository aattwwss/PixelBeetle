#!/usr/bin/env bash
# Dev RabbitMQ for the TigerBeetle CDC job. Uses podman (this machine's
# `docker` symlinks to podman). Declares the durable fanout `tigerbeetle`
# exchange the CDC job publishes to (it must pre-exist).
set -euo pipefail

NAME=pixelbeetle-rabbit
EXCHANGE=tigerbeetle
HTTP_PORT=15672

start() {
  if podman ps --format '{{.Names}}' | grep -qx "$NAME"; then
    echo "rabbitmq already running"
  else
    podman rm -f "$NAME" >/dev/null 2>&1 || true
    # Rootless podman: mount a host dir for /var/lib/rabbitmq with permissive
    # ownership, otherwise the .erlang.cookie is root-owned on the host and the
    # in-container rabbitmq user hits "eacces".
    DATA_DIR="$HOME/.pixelbeetle-rabbitmq"
    mkdir -p "$DATA_DIR"
    # Best-effort: on re-runs the dir is owned by the container's mapped uid
    # and chmod fails with EPERM — ownership is then already correct anyway.
    chmod 0777 "$DATA_DIR" 2>/dev/null || true
    if ! podman run -d --name "$NAME" \
       -v "$DATA_DIR:/var/lib/rabbitmq" \
       -p 5672:5672 -p "$HTTP_PORT:15672" \
       docker.io/library/rabbitmq:3-management >/dev/null; then
      # Transient failures happen right after a rm (port release lag) — retry.
      sleep 3
      podman run -d --name "$NAME" \
        -v "$DATA_DIR:/var/lib/rabbitmq" \
        -p 5672:5672 -p "$HTTP_PORT:15672" \
        docker.io/library/rabbitmq:3-management || { echo "podman run failed" >&2; exit 1; }
    fi
  fi

  echo -n "waiting for rabbitmq"
  for _ in $(seq 1 60); do
    # The management HTTP listener comes up AFTER the Erlang node pings OK,
    # so poll the API itself — a bare ping races it (curl error 56).
    if curl -4 -s -o /dev/null -u guest:guest \
         "http://127.0.0.1:$HTTP_PORT/api/overview"; then
      break
    fi
    echo -n "."
    sleep 1
  done
  echo

  # Declare the durable fanout exchange the CDC job requires. Use 127.0.0.1
  # (not localhost) so curl picks IPv4 — podman's passt only forwards IPv4.
  code=$(curl -4 -s -o /tmp/rabbit_exchange.out -w '%{http_code}' \
    -u guest:guest -X PUT \
    "http://127.0.0.1:$HTTP_PORT/api/exchanges/%2F/$EXCHANGE" \
    -H 'Content-Type: application/json' \
    -d '{"type":"fanout","durable":true,"auto_delete":false}')
  if [ "$code" != "201" ] && [ "$code" != "409" ] && [ "$code" != "204" ]; then
    echo "failed to declare exchange (http $code): $(cat /tmp/rabbit_exchange.out)" >&2
    exit 1
  fi
  echo "exchange '$EXCHANGE' ready (http $code)"

  # Durable sink queue permanently bound to the exchange. The CDC job EXITS
  # with NO_ROUTE if it has something to publish while no queue is bound, so
  # there must always be at least one binding. Consumers use their own
  # exclusive auto-delete queues and get independent fanout copies.
  curl -4 -s -o /dev/null -X PUT \
    "http://127.0.0.1:$HTTP_PORT/api/queues/%2F/$EXCHANGE.sink" \
    -u guest:guest -H 'content-type: application/json' \
    -d '{"durable":true,"auto_delete":false,"arguments":{}}'
  curl -4 -s -o /dev/null -X POST \
    "http://127.0.0.1:$HTTP_PORT/api/bindings/%2F/e/$EXCHANGE/q/$EXCHANGE.sink" \
    -u guest:guest -H 'content-type: application/json' -d '{"routing_key":""}'
  echo "sink queue '$EXCHANGE.sink' bound"
}

stop() {
  podman rm -f "$NAME" >/dev/null 2>&1 || true
  echo "stopped"
}

status() {
  podman ps --filter "name=$NAME"
}

case "${1:-}" in
  start) start ;;
  stop) stop ;;
  status) status ;;
  *) echo "usage: $0 {start|stop|status}" >&2; exit 1 ;;
esac
