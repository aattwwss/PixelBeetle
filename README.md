# PixelBeetle — Canvas Clash

A r/place-style pixel game whose entire state lives in [TigerBeetle](https://github.com/tigerbeetle/tigerbeetle).
Built to showcase two-phase transfers, idempotency, immutable history, CDC, and fault tolerance.

- **Design doc**: [plan.md](plan.md)
- **Stack**: Go everywhere · SSR + DataStar (SSE) · TigerBeetle ×3 · RabbitMQ CDC · built-in load generator

## Status

Core loop + DB-enforced exclusivity + warm-up + CDC are done and live-verified.
See plan.md §0 for the full progress tracker.

## Run (dev, all-native)

```sh
scripts/tigerbeetle.sh start    # TigerBeetle ×3 on :3000-3002
scripts/server.sh start     # game server on :8080 (+ warm-up + CDC)
scripts/cdc.sh start        # tigerbeetle amqp CDC job → RabbitMQ
scripts/rabbitmq.sh status    # RabbitMQ (podman) — auto-started by cdc.sh
```

Web UI: http://localhost:8080. Load test: `go build -o bin/bot ./cmd/bot && ./bin/bot -target http://localhost:8080 -rps 100`.

## Layout

```
cmd/server     game server entry point
cmd/bot        load generator entry point
internal/tbclient  TigerBeetle wrapper + claim construction (shared by server & bots)
internal/game      pixel cache, lock table, claim service
internal/hub       DataStar SSE broadcast hub
internal/web       SSR handlers + templates
internal/replay    CDC consumer (AMQP parsing + dedupe)
internal/warm      boot-time cache rebuild from query_transfers
scripts/           tigerbeetle, server, cdc, rabbitmq
web/static         static assets (incl. vendored datastar.js)
```
