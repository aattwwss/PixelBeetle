# PixelBeetle

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

### Paint mode (draw a blueprint, pixel by pixel)

The bot can paint a text-art blueprint, a hand-composed drawing, or an image
on the canvas through the same claim→confirm cycle, so the history timelapse
shows the picture emerging. See `draw-plan.md` for the blueprint format,
shape syntax, and image pipeline.

```sh
go build -o bin/bot ./cmd/bot
./bin/bot -paint examples/smiley.txt                # text art, centered, 16 workers
./bin/bot -paint examples/tb-box.txt -paint-offset 10,40   # place it
./bin/bot -paint examples/beetle.txt -paint-order random  # developing-photo effect
./bin/bot -paint photo.png                          # image → 16-color conversion
./bin/bot -paint photo.png -inspect > art.txt       # review/export as text art first
./bin/bot -preview examples/smiley.txt -paint-offset 10,40   # overlay on the live canvas, print ASCII, no claims
./bin/bot -draw "rect:1,1,18,9,#4363d8" -draw "text:3,3,TB,#ffd600"   # shapes only
```

`-preview` fetches the current canvas and shows placement + collisions before
painting: lowercase = existing pixels, UPPERCASE = what will be painted,
`X` = overpaint, `.` = empty. Combine with the canvas coordinate readout
(hover the live canvas at `/` for `x,y`) to pick your spot.

Flags: `-paint` (`.txt` art or `.png/.jpg/.jpeg/.gif` image), `-draw`
(repeatable shape specs: `rect`/`fillrect` `x,y,w,h` · `circle` `cx,cy,r` ·
`line` `x0,y0,x1,y1` · `text` `x,y,String` — each followed by `,#hex`),
`-paint-size WxH` (image target box; default fits the grid, aspect-preserving),
`-paint-offset x,y` (default: centered), `-paint-workers N` (default 16; the
shared `-rps` flag acts as a global cap when > 0), `-paint-order
scanline|random`, `-inspect` (print the blueprint as text art and exit),
`-preview` (same inputs as `-paint`, but fetch the canvas, overlay, print
ASCII, place no claims).
Pixels held by other players are retried up to 3× after the claim window,
then skipped.

## Run (deploy, Docker)

`docker-compose.yml` is a deployable, corrected spec for environments with a
real Docker daemon: TigerBeetle ×3 from `ghcr.io/tigerbeetle/tigerbeetle`
(not Docker Hub) + RabbitMQ, all with `network_mode: host` (TigerBeetle
refuses DNS/service-name addresses, so host networking is required).

```sh
docker compose up -d rabbitmq
# wait for RabbitMQ healthy, then start TigerBeetle + the CDC job:
docker compose up -d tigerbeetle-0 tigerbeetle-1 tigerbeetle-2 cdc
# run the game server natively (or containerize it separately):
make serve
```

First RabbitMQ start: `sudo mkdir -p ~/.pixelbeetle-rabbitmq && sudo chown 999:999 ~/.pixelbeetle-rabbitmq`
(rootless containers hit a `.erlang.cookie` permission error otherwise).
See comments in `docker-compose.yml` for caveats (`amqp` subcommand availability
in the image, CDC as a host process).

## Fault-tolerance demo

Kills one of the three TigerBeetle replicas mid-load to show that the quorum
(3 replicas tolerate 1 failure) keeps the game serving with zero lost or
duplicated claims, and that the cluster heals when the replica rejoins:

```sh
scripts/fault-demo.sh
```

It starts the cluster + broker + CDC, runs a bot load, `kill -9`s a follower
replica, confirms claims still succeed while it's down, restarts the replica,
and prints a PASSED transcript.

## Layout

```
cmd/server     game server entry point
cmd/bot        load generator + painter entry point (see Paint mode above)
internal/tbclient  TigerBeetle wrapper + claim construction (shared by server & bots)
internal/game      pixel cache, lock table, claim service
internal/hub       DataStar SSE broadcast hub
internal/web       SSR handlers + templates
internal/replay    CDC consumer (AMQP parsing + dedupe, self-reconnecting)
internal/canvas    packed bitmap encoding
scripts/           tigerbeetle, server, cdc, rabbitmq
web/static         static assets (incl. vendored datastar.js)
```
