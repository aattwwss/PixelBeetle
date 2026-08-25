# TigerBeetle cheat sheet (verified against docs.tigerbeetle.com + tigerbeetle-go v0.17.9)

Fetched 2026-08-26. Focus: what PixelBeetle needs — two-phase transfers,
balance-bound invariants, batching, idempotency, queries.

## The decisive fact for our rewrite: pessimistic pending transfers

From /coding/two-phase-transfers/ ("Interaction with Account Invariants" and
"Pessimistic Pending Transfers"):

> If an account with debits_must_not_exceed_credits has credits_posted = 100
> and debits_posted = 70 and a pending transfer is started causing the account
> to have debits_pending = 50, **the pending transfer will fail** [at creation].
> It will not wait to get to posted status to fail.

**Pending amounts count toward must-not-exceed invariants at create time.**
This is what lets TigerBeetle itself reject a second concurrent claim on a
pixel: fund the pixel with balance=1, claims are debits; the 2nd concurrent
pending overdraws → rejected atomically by the DB. No app lock needed.

Result statuses (Go constants, bindings.go):
- `TransferExceedsCredits` (54) — debit account has
  `debits_must_not_exceed_credits`, `debits_pending + debits_posted +
  transfer.amount > credits_posted`. **Transient**: retrying the same id gives
  the same outcome.
- `TransferExceedsDebits` (55) — mirror image on credit side.
- `TransferPendingTransferExpired` (35) — post/void against an expired pending.
- `TransferCreated` (0xFFFFFFFF) — success.

## Account model for pixel exclusivity (the rewrite)

```
create account pixel_<x>_<y>: flags = {DebitsMustNotExceedCredits}, ledger=1
fund once:                    transfer system_pool -> pixel, amount=1

claim:    PENDING transfer  pixel -> system_pool, amount=1, code=color|base,
          user_data_128=player, timeout=3s
          → 2nd concurrent claim fails at TB with ExceedsCredits ✓
confirm:  ONE batch [post_pending leg, re-fund transfer pool -> pixel amount=1]
          (batches commit atomically; events apply in series)
cancel:   void_pending leg → balance restored automatically
expiry:   timeout elapses → TB restores balance automatically
```

Notes:
- create_accounts and create_transfers are different requests; can't mix in
  one message ("a single request can create multiple transfers but cannot
  create both accounts and transfers"). Account creation then funding =
  two sequential requests.
- Post/void legs: id must be unique/new; pending_id references the pending;
  debit/credit accounts, ledger, code may be zero or MUST match the pending.
  Amount < pending amount = partial post (we always use full/zero).
- A pending can be resolved exactly once: `pending_transfer_already_posted /
  already_voided / expired` otherwise.

## Two-phase essentials (/coding/two-phase-transfers/)

- Pending reserves into `debits_pending`/`credits_pending`; posted fields move
  only on post. Void restores; timeout restores.
- Timeouts are **intervals in seconds**, not absolute timestamps.
- Completing a two-phase transfer creates a NEW transfer (second leg); the
  pending is never modified. All transfers immutable.

## Batching & clients (/coding/requests/)

- Max batch: **8189 events** per request for lookup/create ops.
- "The cluster commits an entire request at once. Events are applied in
  series, such that successive events observe the effects of previous ones."
  → post+re-fund in one batch is safe and atomic.
- **Automatic batching exists in clients**: "The TigerBeetle client should be
  shared across threads… it automatically groups together batches of small
  sizes into one request." Client has at most one in-flight request and
  accumulates while waiting. (This softens the TigerFans lesson: their custom
  LiveBatcher predates/parallels this; still worth measuring, but no hand
  -rolled batching layer needed up front.)
- Requests execute at most once; requests do not time out — clients retry
  internally until answered.

## Idempotency & reliable submission (/coding/reliable-transaction-submission/)

- Transfer/account ids are idempotency keys: resubmitting the same id yields
  `exists` (or created exactly once). Generate ids on the *client* side of the
  API boundary when real clients exist; persist before sending; retry with the
  SAME id.
- Transient errors (exceeded_*) are deterministic per id.

## Queries (/reference/query-filter/, query_transfers/query_accounts)

Filter fields: user_data_128/64/32, ledger, code, timestamp_min/max
(both inclusive), limit (>0, bounded by max message size). Pagination = page
by last result's timestamp (adjust min/max). Ordering follows timestamps.
Useful for boot-time cache warm-up without CDC: query_transfers with
code filter + timestamp ranges.

## Ops notes

- Native binaries preferred over Docker (see scripts/dev-cluster.sh).
- 3 replicas tolerate 1 failure (quorum 2f+1).
- CDC (`tigerbeetle amqp ...`) emits two_phase_pending/posted/voided/expired;
  body carries full transfer + point-in-time balances; use body timestamp
  (ns) not AMQP header (s); single-instance job; at-least-once delivery.
- Go client: cgo static lib; one shared client per process; Submit-style calls
  accept slices → batch naturally.

## Go constants quick reference (bindings.go v0.17.9)

```go
tb.AccountFlags{DebitsMustNotExceedCredits: true}.ToUint16()
tb.TransferFlags{PostPendingTransfer: true}.ToUint16()
tb.TransferCreated        // 0xFFFFFFFF success
tb.TransferExceedsCredits // 54 — invariant would break (our "locked" signal!)
tb.TransferExceedsDebits  // 55
tb.TransferPendingTransferExpired // 35
tb.Uint128 via tb.ToUint128(uint64) / tb.BytesToUint128([16]byte)
// t.ID.Bytes() returns [16]byte (array, not slice)
```
