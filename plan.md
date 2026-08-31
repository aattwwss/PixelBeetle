# PixelBeetle — Implementation Plan

A r/place-style multiplayer pixel game whose entire state lives in **TigerBeetle**.
Primary objective: **showcase TigerBeetle** — two-phase transfers, idempotency,
strictly-ordered immutable history, CDC streaming, and fault tolerance — wrapped
in gameplay that makes those features *visible*.

---

## 0. Progress tracker

_Updated: 2026-08-26._ Repo: `github.com/aattwwss/PixelBeetle`. Run instructions:
`scripts/tigerbeetle.sh start` (TB ×3 native) then `scripts/server.sh start`
(see README).

### Done ✅

- **Project scaffold**: Go module, `cmd/server` + `cmd/bot`, packages
  `internal/{tbclient,game,hub,web,canvas,bot,replay}`; builds clean (`go vet`).
- **Infra (dev)**: native TigerBeetle v0.17.9 cluster, 3 replicas on
  `127.0.0.1:3000-3002` via `scripts/tigerbeetle.sh` (start/stop/status/format).
  *Deviation from §6*: docs explicitly discourage running TB in Docker; image
  lives on ghcr.io (not Docker Hub) and multi-replica compose requires host
  networking (TB validates replica addresses, rejects DNS names).
  docker-compose.yml is reworked for this (2026-08-27) — see Known gaps.
- **Core server**: full two-phase claim flow working end-to-end against the live
  cluster:
  - claim → pending transfer (UUIDv7 id, amount=1, code=`TransferCodeClaim|color`,
    user_data_128=player, timeout=3s) + in-memory lock;
  - confirm → post leg (**must set `PendingID`; code must repeat the pending's
    exactly**) → cache updated, `PixelPainted` broadcast;
  - cancel → void leg → pixel freed;
  - abandon → lock expires from memory after ~3s, TB auto-expires the pending;
  - contested claim returns HTTP 409.
- **Web UI**: SSR grid (html/template) + DataStar SSE hub; delegated click
  handler; painted-cell markup shared between SSR and SSE patches
  (`internal/canvas`). Fixed html/template escaping of cell HTML.
- **Load generator skeleton**: `cmd/bot` with api/direct modes, token-bucket ramp,
  hotspot mode, latency histogram (p50/p99), ~10% deliberate abandonment.
- **Live-verified demo paths**: repaint by another player, reclaim after cancel,
  reclaim after expiry.

### Known gaps 🚧

- **docker-compose.yml reworked** (2026-08-27): now a correct deployable spec —
  `ghcr.io/tigerbeetle/tigerbeetle:0.17.9` (not Docker Hub), `network_mode: host`
  on every service (TB validates replica addresses and refuses DNS names),
  format-if-missing start commands matching `scripts/tigerbeetle.sh`, RabbitMQ
  with a host volume for `/var/lib/rabbitmq` (rootless-podman `.erlang.cookie`
  fix). Dev on this machine still uses the native scripts (`make up`); the
  compose file targets environments with a real Docker daemon.

### Boot-time warm-up + CDC consumer (2026-08-26) ✅

Both build-order steps 4 & 5 are now done and live-verified:

- **Warm-up** (`internal/warm`, `-warmup` flag, default on): pages through
  `query_transfers` (ledger=1, ascending, `TimestampMin` inclusive) and folds
  posted claim legs (`Flags & 0x4`) into the pixel cache. A restart now
  restores the canvas — verified: painted (0,0)/(0,1)/(10,20), restarted,
  `pixels=3` and all cells re-render.
- **CDC consumer** (`internal/replay`, `-cdc-url` flag): consumes the
  `tigerbeetle amqp` stream over RabbitMQ, parses the CDC JSON body (u128/u64
  accepted as decimal strings *or* bare numbers), dedupes by transfer id, and
  applies posted claims via `game.Service.ApplyEvent` (idempotent: no-op on
  the originating instance, paint+broadcast on a second instance).
- **RabbitMQ dev**: `scripts/rabbitmq.sh` (podman, declares the durable
  fanout `tigerbeetle` exchange) and `scripts/cdc.sh` (native
  `tigerbeetle amqp` job).
- **Multi-server sync verified**: ran a second server on :8081 with `-cdc-url`;
  painted on :8080; :8081 caught up via CDC alone (no direct claim).

Ops gotchas discovered (worth knowing before the demo):

- The CDC job exits on `NO_ROUTE` (publish with no queue bound). Fixed on
  2026-08-26: `scripts/rabbitmq.sh` now declares a durable `tigerbeetle.sink`
  queue permanently bound to the exchange (so NO_ROUTE can't happen), and
  `scripts/cdc.sh` runs the job under a restart supervisor as belt-and-
  braces. Verified: paint while no consumer is attached, later consumer still
  receives the full backlog.
- RabbitMQ under rootless podman hits `.erlang.cookie: eacces` unless
  `/var/lib/rabbitmq` is a host volume with permissive ownership (the script
  handles this).
- Management API must be reached via `127.0.0.1`, not `localhost` (podman's
  passt only forwards IPv4; curl otherwise tries `::1` and gets a reset).
- Pixel ids moved into a dedicated 128-bit namespace (`PixelIDMarker` in the
  high 64 bits) — the old `x<<32|y` scheme collided with `SystemPoolID` (1)
  and with the illegal zero id, breaking paints on (0,0) and (0,1).

### Core model v2: DB-enforced exclusivity (2026-08-26) ✅

Rewrote the transfer model so **TigerBeetle itself rejects competing claims**
(docs/tigerbeetle-cheatsheet.md has the full spec):

1. Pixel account created with `debits_must_not_exceed_credits`, funded once
   with exactly 1 unit (deterministic fund-transfer id ⇒ idempotent).
2. Claim = pending debit of that unit. A second concurrent claim fails AT
   CREATION inside TB (`exceeds_credits`) — no app lock decides the winner.
3. Confirm = one linked batch: post leg + re-fund (atomic; unit restored).
4. Cancel/expiry voids the pending — unit restores automatically.

The in-memory lock table remains only as an instant-reject fast path and UI
broadcast trigger; it is never the source of truth. Verified live: race → 409,
confirm paints, repaint works, supersede/cancel/expiry all free the unit,
bots run clean.

Note: the Go client auto-batches concurrent submissions (docs), so the
previously planned custom batching layer was dropped.

### Build order status (§7)

| # | Step | Status |
|---|------|--------|
| 1 | Infra: TB ×3 (+ RabbitMQ) | ✅ TB native scripts + RabbitMQ podman script; compose stale |
| 2 | Core server: accounts, pending→post/void, locks, cache | ✅ done; v2 model puts exclusivity inside TB |
| 3 | Web UI: SSR grid + DataStar SSE | ✅ basic loop works; polish (palette picker, countdown HUD) open |
| 4 | CDC consumer + timelapse view | ✅ CDC live-verified; timelapse redesigned (2026-08-31): separate /history page, every frame = live TB query (anchor checkpoint + ≤1 minute of ledger), no client-side manifest |
| 5 | Cache recovery (snapshot + catch-up on boot) | ✅ PBSSNAP2 snapshot (bitmap + warmTs + anchor grid) → 738ms boot vs 32m full replay on an 18M-transfer ledger; evicted checkpoints spill to an append-only sidecar so old timelapse seeks stay cheap |
| 6 | Load gen + live dashboard | ✅ both done (2026-08-26): bot `-grid` full-canvas capable; server-side atomic counters + bot heartbeat merged into a `metrics` signal broadcast over the SSE hub every 1s; `#metrics-panel` with claims/s, confirms/s, locks, pixels, viewers, bot p50/p99/conflicts |
| 7 | Fault-tolerance demo script (kill replica mid-load) | ✅ `scripts/fault-demo.sh` (2026-08-26): runs load, kill -9 a follower, proves claims confirm through the quorum, restarts + rejoins; kill is PID-based (avoids pkill self-match) |

### Suggested next actions

1. ~~Rework-or-retire docker-compose.yml~~ ✅ done (2026-08-27): ghcr image,
   host networking, RabbitMQ host volume. Remaining polish:
2. Polish: palette color picker (app.js still random), HMAC-sign the player cookie.
3. Slider `__throttle` before a 1M-pixel run.
4. Warmup progress logging (large ledgers make `WarmCache` look hung).

---

## 1. Core model

TigerBeetle is an accounting database, so we store **events (claims), not state**.
Canvas state is derived by replaying posted transfers.

### Accounts

| Account ID        | Purpose                                                        |
|-------------------|----------------------------------------------------------------|
| `pixel_<x>_<y>`   | One account per pixel. Balance = claim counter (free version number). |
| `system_pool`     | Dummy debit side so every transfer balances double-entry style. |

### Transfers

| Field              | Value                                                          |
|--------------------|----------------------------------------------------------------|
| `id`               | **UUIDv7** (time-ordered — good LSM insert locality; also gives idempotency on retries) |
| `debit_account_id` | `system_pool`                                                  |
| `credit_account_id`| `pixel_<x>_<y>`                                                |
| `amount`           | `1` (increments pixel balance → implicit per-pixel version)    |
| `ledger`           | `1` (Canvas)                                                   |
| `code`             | Color chosen by the player (0–255 fits `code`'s range)         |
| `user_data_128`    | Player ID                                                      |
| `flags`            | `pending` initially; then `post_pending_transfer` or `void_pending_transfer` |
| `timeout`          | Lock window in seconds (**3s** default — snappy for players and demo pacing) |

### Lifecycle

1. Client sends `(x, y, color, player_id)` → server mints UUIDv7 transfer id.
2. Server submits **pending** transfer → pixel locked for that player.
3. Within the window:
   - confirm → post pending transfer (`post_pending_transfer`)
   - cancel  → void (`void_pending_transfer`)
   - neither → TB auto-expires it and emits `two_phase_expired` on CDC
     (**no sweeper process needed**).

### Concurrency story (be precise in demos)

"No two players post to the same pixel" is enforced by the **Game Server's
lock check** (is there an unresolved pending claim on this pixel?), not by
TigerBeetle itself — TB would accept two conflicting posts. Single-server mode:
in-memory lock table. Multi-node mode: shared Redis lock. Never claim on slides
that strict serializability alone prevents double claims.

---

## 2. Reading the canvas

- In-memory hash map on the Game Server: `(x,y) → {owner, color, version}`.
- Updated on every posted claim.
- **Cold start**: rebuild from CDC stream (`--timestamp-last=<snapshot_ts>`),
  or from `query_transfers` time-range queries against TigerBeetle directly.
  A fresh server never serves traffic before its cache catches up.
- Clients read only from this cache (sub-millisecond responses).

---

## 3. CDC pipeline & time travel (the killer feature)

Real feature as of TigerBeetle ≥ 0.16.43:

```
./tigerbeetle amqp --addresses=... --cluster=... \
    --host=<rabbitmq-ip>:5672 --vhost=/ --user=... --password=... \
    --publish-exchange=tigerbeetle
```

- Event types map perfectly onto gameplay:
  - `two_phase_pending` → lock started
  - `two_phase_posted`  → claim finalized → paint
  - `two_phase_voided`  → cancelled → unlock
  - `two_phase_expired` → timeout auto-resolved → unlock
- Message body contains full transfer + both accounts with point-in-time balances.
- Replay Service stores posted claims in order; slider scrubs canvas at any
  timestamp `T` (apply all claims with `timestamp <= T`).

Operational notes:
- CDC job is **single-instance** (second instance exits non-zero). Monitor &
  restart; don't promise HA for it in the demo narrative.
- Delivery is **at-least-once** → consumer dedupes by transfer id (falls out of
  our existing idempotency design).
- Use the `timestamp` field **inside the JSON body**, not the AMQP header
  (headers truncate to seconds; body has full nanosecond precision).
- Version discipline: CDC job must not be newer than the cluster. Pin versions
  in setup scripts.

Demo flourish: terminal tailing the RabbitMQ exchange during gameplay — viewers
watch `pending → posted` events scroll as pixels are claimed.

---

## 4. Load generator (first-class component)

Human clicks use ~0% of TB capacity. To actually showcase throughput:

- **Bot swarm**: N scripted agents issuing claims at configurable rate
  (ramp from hundreds → tens of thousands of transfers/sec).
- Each bot uses its own player id; random pixels/colors; some percentage of
  claims deliberately abandoned (exercises pending-expiry path).
- **Live dashboard**: client-side p50/p99 latency, transfers/sec, success/expiry
  counts. Displayed alongside the canvas during the demo.
- Also serves as correctness fuzzer: assert no two posted claims share a pixel
  within a lock window.

---

## 5. Architecture

```
┌─────────┐      ┌────────────────────┐      ┌───────────────────┐
│ Clients │─────▶│ Game Server (Go)   │─────▶│ TigerBeetle       │
│ SSR+DataStar  │ - pixel cache      │      │ cluster (3 repl.) │
└────┬────┘      │ - locks, logic     │      └─────────┬─────────┘
     │ SSE       └────────────────────┘                │
┌───────────┐                                         │ CDC (AMQP)
│ Bot swarm ├────────────────────────────▶ (same API) │
└───────────┘                                         ▼
                                             ┌──────────────────┐
                 ┌───────────────────┐       │ RabbitMQ         │
                 │ Replay Service    │◀──────└──────────────────┘
                 │ - dedupe by tx id │
                 │ - ordered log     │──▶ Time-travel slider UI
                 └───────────────────┘
```

---

## 6. Tech stack

**Fully SSR in Go. No SPA framework, no client-side state management.**

| Component     | Choice                                        |
|---------------|-----------------------------------------------|
| Language       | Go everywhere (server, CDC/replay service, bots, dashboard) — `tigerbeetle/go` is a first-class client |
| Templating    | SSR via `templ` (typed components) or `html/template`; hypermedia responses only |
| Reactivity    | **DataStar** (`datastar-go` SDK): SSE-driven signals + fine-grained DOM patching |
| TigerBeetle   | Docker, 3 replicas, version ≥ 0.16.43         |
| RabbitMQ      | Docker                                        |
| Load gen      | Same Go codebase, headless goroutine workers  |
| Dashboard     | Server-rendered metrics page updated over SSE |

### Why DataStar over htmx

- The gameplay loop is dominated by **server→client push**: every posted claim,
  void, and expiry must reach all viewers immediately. DataStar's core primitive
  is a single long-lived SSE stream carrying fine-grained patches/signals.
  htmx is request-driven; its SSE support is an extension that swaps response
  fragments — workable, but we'd be swimming against its grain.
- One SSE endpoint can multiplex everything: pixel updates, lock states,
  confirm-countdowns, dashboard metrics.
- Native browser reconnect semantics for free; on reconnect the server sends a
  full canvas-diff patch (client never needs a manual resync protocol).

### Rendering strategy & constraint

SSR means DOM pixels, not `<canvas>`. Budget accordingly:

- Grid size capped at ~**128×128 = 16,384 cells** — each cell an SSR-rendered
  `<div>` colored via CSS custom property/signal. DataStar patches only changed
  cells (broadcast fan-out of tiny diffs).
- A claim broadcasts one small patch event per cell — at even 10k claims/sec
  this is trivial for the server; the browser is the bottleneck, hence the cap.
- If we ever want a huge canvas later: hybrid (SSR shell + `<canvas>` overlay
  hydrated from an SSE snapshot), out of scope for v1.

### SSE topology

- `GET /sse` — one EventSource per client. Server-side **broadcast hub**
  (per-instance channel registry) fans out events: `pixel-posted`,
  `pixel-locked`, `pixel-unlocked`, `metrics`.
- Hub input comes directly from the Game Server's post/void/expiry handling
  (low latency path); CDC remains the durable/replay path. Both are fed from
  the same ordered transfer stream so they cannot diverge.
- Multi-node: hub subscribes to RabbitMQ instead of in-process events
  (same message shape). v1 runs single-node.

---

## 7. Build order

1. **Infra**: docker-compose with TB ×3 + RabbitMQ; script `tigerbeetle amqp` startup; health checks.
2. **Core server**: create accounts (pixels lazily), pending→post/void flow, lock check, in-memory cache.
3. **Web UI**: SSR grid (`templ`) + DataStar SSE wiring: click → lock cell → countdown signal → confirm/cancel buttons → paint. Live updates fanned out via the SSE hub.
4. **CDC consumer + replay service**: consume, dedupe, ordered log, `/replay?ts=T` endpoint; slider UI.
5. **Cache recovery**: snapshot timestamp + catch-up-from-CDC on boot.
6. **Load generator + dashboard**: ramp control, latency histogram, error counters.
7. **Fault-tolerance demo script**: kill a replica mid-load; show zero lost/duplicated claims; restart CDC job after crash and verify resume from last acked timestamp.

## 8. Demo checklist

- [ ] Two browsers racing for one pixel → second sees "locked".
- [ ] Abandon a claim → watch `two_phase_expired` appear in the queue tail.
- [ ] Retry a claim after network blip → no double paint (UUIDv7 idempotency).
- [ ] Slider scrub through entire canvas history.
- [ ] Bot swarm at high RPS with live latency graph.
- [ ] `kill -9` a TB replica mid-load → game continues uninterrupted.

---

## 9. State inventory — why there's no SQL database

The entire application is **one accounting database plus a read cache**. Every
piece of state falls into "durable events" (TigerBeetle) or "derived/ephemeral"
(memory). No traditional database needed for v1.

| Concern | Home | Why no RDBMS |
|---|---|---|
| Pixel state (color, owner) | In-memory cache, rebuilt from TB/CDC | Derived data; TB is the source of truth |
| Full claim history | TigerBeetle itself | Immutable, timestamped, queryable forever (`query_filter`). A separate replay log in Postgres would be redundant — TB *is* the log |
| Replay slider | Query TB directly / consume CDC on demand | Nothing to persist |
| Pending locks | Game server memory (single node) | Ephemeral by definition; the pending transfer in TB is the durable record |
| Player identity | Signed cookie with UUIDv7 player id | Demo has no passwords/profiles |
| Sessions | Stateless cookies | Nothing to store |
| Rate limiting | In-memory token bucket per player | Ephemeral |
| Leaderboards / stats | In-memory aggregates fed by CDC | Recomputable any time from TB |

Demo talking point: *"the entire state model is one accounting database plus a
read cache."*

### What would force adding a real DB (documented, not built)

1. Real user accounts (emails, password hashes, OAuth).
2. Moderation/bans — mutable judgments that aren't ledger events.
3. Multi-node deployment → Redis for shared lock/cache coordination.
4. Rich mutable player profiles (avatars, bios).

All four are out of scope; they exist on this list so the omission is explicit.

### 4.1 Load generator design

- **Packaging**: same repo, separate binary `cmd/bot`. Run locally during demos;
  no containers needed.
- **Live control**: `POST /admin/bots {"rps": N}` endpoint lets you ramp load
  mid-demo instead of pre-scripting it. CLI flags for initial config:
  `-target -rps -duration -ramp -mode`.
- **Agent model**: pool of goroutine agents, each with its own UUIDv7 player id.
  Loop: pick pixel → submit claim → ~90% confirm after 100–500ms think delay,
  ~10% abandon (exercises `two_phase_expired` auto-expiry).
- **Pacing**: shared global token bucket = single knob; linear ramp over
  `-ramp` duration for clean latency-vs-load curves.
- **Modes**:
  | Mode | Path | Purpose |
  |------|------|---------|
  | `api` (default) | HTTP → Game Server | Honest end-to-end number: locks, cache, SSE fan-out |
  | `direct` | Shared `tbclient` wrapper → TB | Raw DB ceiling without app overhead |
  Both modes build transfers via the same claim-construction code as the server.
  Side-by-side numbers = "app adds X%; the DB was never the bottleneck."
  Plus `--hotspot <x,y>`: all agents fight over one pixel (max contention demo).
- **Metrics**: per-op latency histograms, atomically aggregated (p50/p99,
  ops/sec, confirmed/voided/expired/conflicts/errors). Exposed via `/metrics`
  scrape AND pushed onto the SSE hub — dashboard is just another SSR page.
- **Shutdown**: stop admitting, resolve in-flight pendings (confirm or void),
  flush final histogram JSON. Never leave dangling pending transfers.
- Caveat: at high RPS the browser rendering SSE patches is the bottleneck, not
  any backend tier — hence the 128×128 grid cap. Keep dashboard closed during
  max-load runs.
