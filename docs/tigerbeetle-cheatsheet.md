# TigerBeetle cheatsheet (verified against docs.tigerbeetle.com + go client v0.17.9)

Fetched 2026-08-26. Source pages: `/concepts/` (via `/coding/data-modeling`),
`/coding/two-phase-transfers`, `/coding/requests`, `/coding/time`,
`/coding/reliable-transaction-submission`, `/coding/recipes/balance-bounds`,
`/reference/account`, `/reference/transfer`, `/reference/requests/create_transfers`.
Go bindings cross-checked against `github.com/tigerbeetle/tigerbeetle-go@v0.17.9`
(`bindings.go`, `tb_client.go`, `tb_client_test.go`).

## Corrections to our earlier assumptions

- **Error name is `exceeds_credits`, not `exceeded_credits`.** Go constant:
  `TransferExceedsCredits = 54` (debit side), `TransferExceedsDebits = 55`.
- **The Go client does NOT use a sparse results array.** `CreateTransfers`
  returns `results[i]` ↔ `transfers[i]`, same length and order, including
  failures (`tb_client_test.go` asserts `results[0].Status ==
  TransferLinkedEventFailed` etc.). The earlier worry that "Reserved holds the
  failing index" was wrong for this version.
- **Batch-of-1 is mostly a non-issue for us.** The Go client auto-batches:
  one shared client, at most one in-flight request, and it *accumulates*
  concurrent submissions while awaiting the previous reply. Our many
  concurrent `Claim()` goroutines sharing one client already pack. No custom
  chained-batcher needed (the reference.md TigerFans slowdown was a Python
  event-loop artifact, not a TB requirement).
- **Pendings are pessimistic**: balance invariants are checked at pending
  *creation*, not post time. This is what makes the pixel-semaphore design
  work — a second claim fails immediately with `exceeds_credits`.

## Account flags (the ones that matter)

- `debits_must_not_exceed_credits` — reject a transfer when
  `debits_pending + debits_posted + transfer.amount > credits_posted`.
  (Note: only *posted* credits count as spendable; pending credits do not.)
- `credits_must_not_exceed_debits` — the mirror.
  Mutually exclusive with the above.
- `history` — retain balance history; required for `get_account_balances`.
- `closed` — reject new transfers except voiding still-pending ones.
- `linked` — this event commits iff the next one in the batch does
  (chained; last link has no flag).
- `imported` — client-defined timestamps (avoid unless importing history).

Balance fields (all start 0, updated only by transfers):
`debits_pending` (reserved by pending debits), `debits_posted`,
`credits_pending`, `credits_posted`. Pending reserves are released on
post/void/expire.

## Two-phase transfers

- `flags.pending` reserves the amount into the debit account's
  `debits_pending` / credit account's `credits_pending`; posted balances
  untouched.
- Resolve with a NEW transfer (immutable history) carrying:
  - `post_pending_transfer` + `pending_id` — post. Amount may be `0` or
    `AMOUNT_MAX` (= full) or a partial amount.
  - `void_pending_transfer` + `pending_id` — void. Amount must be `0` or
    exactly the pending's amount.
  - `debit_account_id` / `credit_account_id` / `ledger` / `code` may be
    zero, else must match the pending exactly (we hit this: code must repeat).
- `timeout` is an interval in **seconds**, measured from arrival at the
  primary (not client-supplied absolute time). Passed as `0` normally;
  the cluster assigns the timestamp.
- A pending resolves exactly once; resolving twice yields
  `pending_transfer_already_posted` / `..._already_voided` /
  `pending_transfer_expired` (Go: 33 / 34 / 35).
- Timeout expiry frees the reserved amount (CDC emits `two_phase_expired`).

## The pixel-semaphore design (our rewrite target)

Enforce "one claim per pixel at a time" inside TigerBeetle, not in memory:

1. **Pixel account**: `flags.debits_must_not_exceed_credits`.
2. **Fund once**: posted credit of 1 into the pixel (debit `system_pool`).
   Use a deterministic fund-transfer id (derived from pixel id) so
   re-running after a restart is idempotent (`exists` = ok).
3. **Claim** = pending transfer `debit pixel 1, credit system_pool 1`.
   - First claim: `0 + 0 + 1 > 1`? no → ok, `debits_pending = 1`.
   - Concurrent second claim: `1 + 0 + 1 > 1`? yes → `exceeds_credits`.
     **The database rejects the contender; no app-level lock needed.**
4. **Confirm** = one linked batch of two transfers:
   - `post_pending_transfer` (posts the claim),
   - re-fund: `debit system_pool 1, credit pixel 1` (plain, posted).
   Net: `credits_posted += 1`, `debits_posted += 1` → still exactly 1
   spendable unit. `debits_posted` = version counter.
5. **Cancel** = `void_pending_transfer` (restores the unit).
6. **Expire** = TB auto-voids; same effect as cancel.

Cost vs current model: +1 transfer per confirm (re-fund) and a one-time
fund per pixel. `system_pool` drifts by −1 per pixel (offset by pixels'
+1; double-entry sums stay consistent).

### Open question to verify live

Whether an *expired-but-not-yet-observed* pending still holds its
reservation when the very next claim lands right at the timeout boundary.
If TB reaps lazily, a claim at t+3.001s may get one `exceeds_credits`;
retry with a fresh UUIDv7 succeeds. Handle by treating `exceeds_credits`
as "retry/contended" and surfacing a clear 409, not an internal error.

## Batching & throughput

- Max batch: **8189 events** per request type (`create_transfers` included).
- **One client per process, shared across goroutines.** The client holds at
  most one in-flight request and packs concurrent submissions while waiting.
- Do not artificially delay to build batches — let concurrency do it.
- All events in a request commit atomically (all-or-nothing), though
  individual events can fail (non-linked).

## Idempotency & reliable submission

- The *client* (browser/app) generates the transfer/account id and persists
  it before sending; retries reuse the same id.
- Re-submitting the same id returns `exists` (Go `TransferExists = 46`,
  `TransferCreated = 0xFFFFFFFF`), or `*ExistsWithDifferent*` (36–45, 67)
  if fields differ.
- Requests do not time out client-side; the client keeps retrying until a
  reply. So network errors are safe to retry with the same id.

## Statuses we care about (Go v0.17.9)

| Constant | Value | Meaning |
|---|---|---|
| `TransferCreated` | `0xFFFFFFFF` | success |
| `TransferExists` | `46` | idempotent success |
| `TransferExceedsCredits` | `54` | contention — overdraw on debit side |
| `TransferExceedsDebits` | `55` | contention — credit side |
| `TransferPendingTransferAlreadyPosted` | `33` | post retried |
| `TransferPendingTransferAlreadyVoided` | `34` | void retried |
| `TransferPendingTransferExpired` | `35` | resolved by timeout |

Result struct is `{Timestamp uint64, Status uint32, Reserved uint32}`, indexed
1:1 with the request slice.

## Go client surface (what we wrap)

- `Client` interface: `CreateAccounts`, `CreateTransfers`, `LookupAccounts`,
  `LookupTransfers`, `GetAccountTransfers`, `GetAccountBalances`,
  `QueryAccounts`, `QueryTransfers`, `Nop`, `Close`.
- `NewClient(clusterID Uint128, addresses []string)`.
- `Uint128` is `__uint128_t` (no named High/Low fields); build via
  `ToUint128`, `BytesToUint128`, `HexStringToUint128`, `ID()`.
- Requires cgo + gcc to build (links static `libtb_client_*.a`).

## Not re-verified this round (existing plan knowledge, see plan.md §3)

CDC message internals were verified in an earlier read (see below for the
summary); query_filter pagination summarized from reference pages.

## Queries (/reference/query-filter/, query_transfers/query_accounts)

Filter fields: user_data_128/64/32, ledger, code, timestamp_min/max
(both inclusive), limit (>0, bounded by max message size). Pagination = page
by last result's timestamp (adjust min/max). Ordering follows timestamps.
Useful for boot-time cache warm-up without CDC: query_transfers with
code filter + timestamp ranges.

## Ops notes

- Native binaries preferred over Docker (see scripts/tigerbeetle.sh).
- 3 replicas tolerate 1 failure (quorum 2f+1).
- CDC (`tigerbeetle amqp ...`) emits two_phase_pending/posted/voided/expired;
  body carries full transfer + point-in-time balances; use body timestamp
  (ns) not AMQP header (s); single-instance job; at-least-once delivery.
- Go client: cgo static lib; one shared client per process; Submit-style calls
  accept slices → batch naturally.

## Go constants quick reference (bindings.go v0.17.9)
