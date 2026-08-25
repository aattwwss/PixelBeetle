# PixelBeetle — Canvas Clash

A r/place-style pixel game whose entire state lives in [TigerBeetle](https://github.com/tigerbeetle/tigerbeetle).
Built to showcase two-phase transfers, idempotency, immutable history, CDC, and fault tolerance.

- **Design doc**: [plan.md](plan.md)
- **Stack**: Go everywhere · SSR + DataStar (SSE) · TigerBeetle ×3 · RabbitMQ CDC · built-in load generator

## Status

🚧 Scaffolding in progress. See plan.md §7 for the build order.

## Layout

```
cmd/server     game server entry point
cmd/bot        load generator entry point
internal/tbclient  TigerBeetle wrapper + claim construction (shared by server & bots)
internal/game      pixel cache, lock table, claim service
internal/hub       DataStar SSE broadcast hub
internal/web       SSR handlers + templates
internal/replay    CDC consumer (replay/time-travel)
web/static         static assets (incl. vendored datastar.js)
```
