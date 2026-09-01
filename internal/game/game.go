// Package game holds PixelBeetle application logic: the pixel read cache,
// the pending-lock table, and the claim service coordinating TigerBeetle with
// the SSE hub.
package game

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	tb "github.com/tigerbeetle/tigerbeetle-go"

	"pixelbeetle/internal/hub"
	"pixelbeetle/internal/replay"
	"pixelbeetle/internal/tbclient"
)

var (
	ErrLockedByOther = errors.New("pixel locked by another player")
	ErrUnknownClaim  = errors.New("unknown claim")
	ErrOutOfBounds   = errors.New("pixel coordinates out of bounds")
)

// Pixel is the derived canvas state served to clients.
type Pixel struct {
	Color uint8
}

// lock is an in-memory gate on top of the durable pending transfer in TB.
type lock struct {
	player  uuid.UUID
	expires time.Time
}

type ClaimMeta struct {
	X, Y   uint32
	Color  uint8
	Player uuid.UUID
}

type Service struct {
	mu             sync.Mutex
	gridW          uint32
	gridH          uint32
	bmp            []byte // standing canvas bitmap, row-major, 0=empty / 1..16 = color+1
	locks          map[uint64]lock
	claims         map[[16]byte]ClaimMeta   // claim transfer id -> meta, until resolved
	byPlayer       map[uuid.UUID][16]byte   // player -> their single active claim
	created        map[uint64]struct{}      // pixel accounts already ensured in TB
	allCreated     bool                     // true once every pixel account is eagerly created+funded
	firstPaintMs   int64                    // earliest paint (ms) — history timeline start
	lastPaintMs    int64                    // latest paint (ms) — history timeline end
	ag             anchorGrid               // 1-minute checkpoint bitmaps for the history view
	anchorPath     string                   // sidecar file holding evicted checkpoint bitmaps ("" = none)
	anchorFile     *os.File                 // sidecar append handle (lazy, O_APPEND)
	anchorBlobs    map[uint64]anchorBlobLoc // hash -> blob location in the sidecar (written or scanned)
	lastAnchorHash uint64                   // hash of the immediately preceding record (consecutive stamp)
	warmTs         uint64                   // watermark: newest folded state ts (ns); CDC events at/below it are redeliveries; snapshot ticker uses it as a dirty signal
	warmed         bool                     // true once WarmCache completed; only a fully-warmed process may write snapshots
	snapshot       string                   // on-disk snapshot path ("" = full replay every boot)
	hub            *hub.Hub
	tb             *tbclient.Client
	log            *slog.Logger
	sysOnce        sync.Once
	metrics        metrics
}

func New(w, h uint32, tb *tbclient.Client, hub *hub.Hub, log *slog.Logger) *Service {
	return &Service{
		gridW:    w,
		gridH:    h,
		bmp:      make([]byte, int(w)*int(h)),
		locks:    make(map[uint64]lock),
		claims:   make(map[[16]byte]ClaimMeta),
		byPlayer: make(map[uuid.UUID][16]byte),
		created:  make(map[uint64]struct{}),
		hub:      hub,
		tb:       tb,
		log:      log,
	}
}

func pack(x, y uint32) uint64 { return uint64(x)<<32 | uint64(y) }

// Claim attempts to lock (x,y) for player. Durable state lands in TigerBeetle
// as a pending transfer; the lock table only gates concurrent attempts.
// A player holds at most ONE active claim: claiming again voids the previous
// pending transfer first (server-enforced invariant, clients can't forget).
func (s *Service) Claim(player uuid.UUID, x, y uint32, color uint8) ([16]byte, error) {
	if x >= s.gridW || y >= s.gridH {
		return [16]byte{}, ErrOutOfBounds
	}
	s.ensureSystemPool()
	s.ensurePixel(x, y)

	key := pack(x, y)
	s.mu.Lock()
	if l, ok := s.locks[key]; ok && time.Now().Before(l.expires) && l.player != player {
		s.mu.Unlock()
		return [16]byte{}, ErrLockedByOther
	}
	// Supersede any prior claim held by this player. The old claim's lock is
	// remembered so a failed submit below can restore it exactly.
	var old ClaimMeta
	oldID := [16]byte{}
	hadOld := false
	hadLock := false
	var oldLock lock
	if id, ok := s.byPlayer[player]; ok {
		if m, ok := s.claims[id]; ok {
			oldID, old, hadOld = id, m, true
			if l, ok := s.locks[pack(m.X, m.Y)]; ok {
				oldLock, hadLock = l, true
			}
			s.vacate(id, pack(m.X, m.Y))
		}
	}
	t := tbclient.NewClaim(x, y, color, player)
	id := t.ID.Bytes()
	s.locks[key] = lock{player: player, expires: time.Now().Add(time.Duration(tbclient.ClaimTimeoutSeconds) * time.Second)}
	s.claims[id] = ClaimMeta{X: x, Y: y, Color: color, Player: player}
	s.byPlayer[player] = id
	s.mu.Unlock()

	// TigerBeetle is the source of truth: if another player's pending holds
	// the pixel's unit, THIS submit fails with exceeds_credits — no app-level
	// lock decides the winner.
	if err := s.tb.Submit(t); err != nil {
		s.mu.Lock()
		delete(s.locks, key)
		delete(s.claims, id)
		if hadOld {
			// The superseded claim was vacated above; restore it so the player
			// keeps their previous (still-durable) pending claim instead of
			// having it silently destroyed by a failed submit.
			s.claims[oldID] = old
			s.byPlayer[player] = oldID
			if hadLock {
				s.locks[pack(old.X, old.Y)] = oldLock
			}
		} else {
			delete(s.byPlayer, player)
		}
		s.mu.Unlock()
		if errors.Is(err, tbclient.ErrPixelLocked) {
			s.metrics.conflicts.Add(1)
			return [16]byte{}, ErrLockedByOther
		}
		s.metrics.errors.Add(1)
		return [16]byte{}, err
	}
	s.metrics.claims.Add(1)

	// Void the superseded pending transfer. If this fails the transfer still
	// self-expires in TB after its timeout; the UI is already consistent.
	if hadOld {
		if err := s.tb.Submit(tbclient.BuildVoid(tb.BytesToUint128(oldID))); err != nil {
			if errors.Is(err, tbclient.ErrClaimExpired) {
				// TB already voided the expired pending and freed the old cell —
				// broadcast the unlock so clients clear the stale yellow box (the
				// reaper never sees this lock again, it was vacated above).
				s.hub.BroadcastUnlock(old.X, old.Y)
				s.log.Debug("superseded claim already expired in TB")
			} else {
				s.log.Warn("failed to void superseded claim", "err", err)
			}
		} else {
			s.hub.BroadcastUnlock(old.X, old.Y)
		}
	}

	s.hub.BroadcastLock(x, y)
	return id, nil
}

// vacate removes a claim and its lock + player index. Caller holds s.mu.
func (s *Service) vacate(claimID [16]byte, key uint64) {
	m, ok := s.claims[claimID]
	if !ok {
		return
	}
	if cur, ok := s.byPlayer[m.Player]; ok && cur == claimID {
		delete(s.byPlayer, m.Player)
	}
	delete(s.claims, claimID)
	if l, ok := s.locks[key]; ok && l.player == m.Player {
		delete(s.locks, key)
	}
}

// Confirm posts the pending transfer, painting the pixel permanently.
func (s *Service) Confirm(player uuid.UUID, claimID [16]byte) error {
	meta, err := s.resolve(player, claimID)
	if err != nil {
		return err
	}
	confirm := tbclient.BuildConfirm(tb.BytesToUint128(claimID), meta.X, meta.Y)
	if err := s.tb.SubmitBatch(confirm); err != nil {
		if errors.Is(err, tbclient.ErrClaimExpired) {
			// The pending timed out in TB before this confirm landed — TB already
			// voided it and freed the cell. Expected under load, not an error.
			s.metrics.expires.Add(1)
			return err
		}
		s.metrics.errors.Add(1)
		return err
	}
	s.metrics.confirms.Add(1)

	// Anchor checkpoints must see the canvas BEFORE this paint lands, so a
	// minute-boundary bitmap never contains the paint that crossed it.
	// The wall clock is the boundary basis (SubmitBatch doesn't return the
	// post leg's TB timestamp). Skew vs. TB's clock is safe in both
	// directions: an anchor stamped before a paint's TB ts makes the fold
	// re-apply that paint (idempotent, same color); an anchor stamped after
	// it already contains the paint in its bitmap, so the fold simply
	// doesn't need to re-apply. Worst case is a few-ms "paint appears a
	// frame early/late" at one boundary — invisible at 1-minute granularity.
	nowNs := time.Now().UnixNano()
	s.mu.Lock()
	s.ag.syncTo(nowNs, s.bmp)
	key := pack(meta.X, meta.Y)
	s.bmp[int(meta.Y)*int(s.gridW)+int(meta.X)] = meta.Color%16 + 1
	s.notePaint(nowNs / 1_000_000)
	// Advance the watermark so the snapshot ticker's dirty-check fires.
	// SubmitBatch doesn't return the post leg's TB timestamp, so the server
	// wall clock is the proxy; on a single-host demo TB's clock tracks ours,
	// and ApplyEvent below only drops CDC redeliveries of events the cache
	// already applied anyway.
	if nowNs > int64(s.warmTs) {
		s.warmTs = uint64(nowNs)
	}
	s.vacate(claimID, key)
	s.mu.Unlock()

	// The pixel is now painted, so its pending lock is released — clear the
	// lock overlay in the same flush as the paint.
	s.hub.BroadcastUnlock(meta.X, meta.Y)
	s.hub.BroadcastPaint(meta.X, meta.Y, meta.Color)
	return nil
}

// Cancel voids the pending transfer early; TB expiry handles the silent case.
func (s *Service) Cancel(player uuid.UUID, claimID [16]byte) error {
	meta, err := s.resolve(player, claimID)
	if err != nil {
		return err
	}
	void := tbclient.BuildVoid(tb.BytesToUint128(claimID))
	if err := s.tb.Submit(void); err != nil {
		return err
	}
	s.metrics.cancels.Add(1)
	s.mu.Lock()
	s.vacate(claimID, pack(meta.X, meta.Y))
	s.mu.Unlock()
	s.hub.BroadcastUnlock(meta.X, meta.Y)
	return nil
}

// resolve validates ownership and returns the claim. Callers remove the
// claim from the registry afterwards via vacate.
func (s *Service) resolve(player uuid.UUID, claimID [16]byte) (ClaimMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.claims[claimID]
	if !ok || m.Player != player {
		return ClaimMeta{}, ErrUnknownClaim
	}
	return m, nil
}

// Snapshot returns the currently painted pixels for SSR rendering / SSE
// resync. Derived from the standing bitmap — the single source of truth (the
// old pixels map duplicated it and had to be kept in lockstep).
func (s *Service) Snapshot() map[uint64]Pixel {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[uint64]Pixel, s.paintedCount())
	for i, v := range s.bmp {
		if v > 0 {
			out[pack(uint32(i%int(s.gridW)), uint32(i/int(s.gridW)))] = Pixel{Color: v - 1}
		}
	}
	return out
}

// paintedCount returns how many cells are painted. Caller must hold s.mu.
func (s *Service) paintedCount() int {
	n := 0
	for _, v := range s.bmp {
		if v > 0 {
			n++
		}
	}
	return n
}

// SnapshotBmp returns the full canvas as a base64 packed bitmap (one byte per
// cell; 0 = empty, 1..16 = palette color + 1) plus the currently locked cells.
// It's the SSE connect payload and the SSR initial state.
func (s *Service) SnapshotBmp() (string, [][2]uint32) {
	now := time.Now()
	var locks [][2]uint32

	s.mu.Lock()
	b64 := base64.StdEncoding.EncodeToString(s.bmp)
	for k, l := range s.locks {
		if now.Before(l.expires) {
			locks = append(locks, [2]uint32{uint32(k >> 32), uint32(k & 0xffffffff)})
		}
	}
	s.mu.Unlock()

	return b64, locks
}

// Grid returns the canvas dimensions.
func (s *Service) Grid() (uint32, uint32) { return s.gridW, s.gridH }

// notePaint widens the timeline bounds to include a paint at ms.
// Caller holds s.mu.
func (s *Service) notePaint(ms int64) {
	if s.firstPaintMs == 0 || ms < s.firstPaintMs {
		s.firstPaintMs = ms
	}
	if ms > s.lastPaintMs {
		s.lastPaintMs = ms
	}
}

// HistoryMeta returns the timelapse timeline bounds: earliest paint, latest
// paint (ms since epoch) and the anchor interval the slider quantizes to.
// (0, 0, 0) when nothing has been painted yet.
func (s *Service) HistoryMeta() (minMs, maxMs, intervalMs int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstPaintMs, s.lastPaintMs, anchorIntervalMs
}

// FrameAt renders the canvas as it looked at tsMs (ms since epoch) by folding
// the nearest one-minute anchor bitmap with the TigerBeetle transfers in
// (anchor, tsMs] — a bounded, live query per request; nothing is cached
// client-side or preloaded. Seeks newer than the latest paint return the
// current canvas. The second return is the effective frame timestamp
// (clamped to the latest paint).
func (s *Service) FrameAt(tsMs int64) (string, int64, error) {
	// Copy the fold starting-point under the lock; the TB query runs without it.
	s.mu.Lock()
	start := make([]byte, len(s.bmp))
	if tsMs >= s.lastPaintMs {
		// At/after the newest paint: the standing bitmap IS the answer.
		copy(start, s.bmp)
		eff := s.lastPaintMs
		s.mu.Unlock()
		return base64.StdEncoding.EncodeToString(start), eff, nil
	}
	var fromNs uint64
	seekStart := time.Now()
	defer func() {
		s.log.Debug("frame seek done", "tsMs", tsMs, "elapsed", time.Since(seekStart).Round(time.Millisecond))
	}()
	if a, startNs, ok := s.ag.checkpoint(tsMs); ok {
		// Start from the newest RAM checkpoint at/before tsMs; the delta query
		// then covers exactly (checkpoint, tsMs] — at most one minute of events.
		copy(start, a)
		fromNs = startNs
	} else if ref, ok := s.ag.sidecarAt(tsMs); ok {
		// Older than the RAM window: read the evicted checkpoint from the
		// sidecar file (one pread) and fold forward from there.
		blob, err := s.readAnchorBlobLocked(ref)
		if err != nil {
			s.mu.Unlock()
			return "", 0, fmt.Errorf("history: sidecar blob: %w", err)
		}
		copy(start, blob)
		fromNs = uint64(ref.TsMs) * 1_000_000
	} else {
		// No checkpoint at all for this era (sidecar missing/truncated):
		// fold from the very beginning — O(entire history), minutes on a
		// large ledger. Logged loudly so the gap gets noticed.
		s.log.Warn("history seek predates every checkpoint; folding full ledger", "tsMs", tsMs)
	}
	s.mu.Unlock()

	toNs := uint64(tsMs)*1_000_000 + 999_999
	if _, err := s.foldFrom(fromNs, maxFramePages, func(t tb.Transfer) bool {
		if t.Timestamp > toNs {
			return true // stop: past the frame's window
		}
		if x, y, color, ok := tbclient.IsPostedClaim(t, s.gridW, s.gridH); ok {
			start[int(y)*int(s.gridW)+int(x)] = byte(color%16 + 1)
		}
		return false
	}); err != nil {
		return "", 0, err
	}
	return base64.StdEncoding.EncodeToString(start), tsMs, nil
}

// ApplyEvent ingests a CDC event (replay.Sink). It only reacts to posted
// claims, painting the cell if its color changed — which makes it idempotent
// on the instance that originated the claim (same color ⇒ no-op) and correct
// on a second instance consuming the stream (stale color ⇒ paint + broadcast).
func (s *Service) ApplyEvent(ev replay.Event) {
	if ev.Type != replay.TypePosted {
		return
	}
	if ev.X >= s.gridW || ev.Y >= s.gridH {
		s.log.Warn("CDC event outside the grid", "x", ev.X, "y", ev.Y)
		return
	}
	s.mu.Lock()
	if ev.Timestamp <= s.warmTs {
		// Already folded into the cache by warm-up: a redelivery, not a new
		// paint. (Order-sensitivity: a stale redelivery with a different color
		// could regress a newer paint — correctness leans on the replay sink's
		// transfer-id dedupe.)
		s.mu.Unlock()
		return
	}
	// Advance the checkpoint grid before mutating the bitmap, for the same
	// reason as in Confirm: a boundary bitmap must not contain its trigger.
	s.ag.syncTo(int64(ev.Timestamp), s.bmp)
	idx := int(ev.Y)*int(s.gridW) + int(ev.X)
	if s.bmp[idx] == ev.Color%16+1 {
		s.mu.Unlock()
		return // idempotent no-op on the instance that already painted it
	}
	s.bmp[idx] = ev.Color%16 + 1
	// The watermark doubles as the snapshot-ticker dirty signal: an applied
	// event must bump it so the periodic save actually fires (TB timestamps
	// are monotonic, so max() is a move-forward, never a regress).
	if ev.Timestamp > s.warmTs {
		s.warmTs = ev.Timestamp
	}
	s.notePaint(int64(ev.Timestamp / 1_000_000))
	s.mu.Unlock()
	s.hub.BroadcastPaint(ev.X, ev.Y, ev.Color)
}

// ReapExpired drops stale in-memory locks. TigerBeetle auto-expires the
// underlying pending transfers (emitting two_phase_expired over CDC), so this
// only unblocks UI claiming — no DB work required.
func (s *Service) ReapExpired() {
	now := time.Now()
	n := 0
	var unlocked [][2]uint32 // reaped cells to broadcast after releasing mu
	s.mu.Lock()
	for k, l := range s.locks {
		if now.After(l.expires) {
			// The pending transfer self-expires in TB; drop the local claim so
			// a late confirm gets a clean 409 instead of posting a dead pending.
			if id, ok := s.byPlayer[l.player]; ok {
				if m, ok := s.claims[id]; ok && pack(m.X, m.Y) == k {
					s.vacate(id, k)
				}
			}
			delete(s.locks, k)
			n++
			// Broadcast so clients stop rendering the cell as locked — without
			// this an expired claim stays visually stuck until someone repaints it.
			unlocked = append(unlocked, [2]uint32{uint32(k >> 32), uint32(k & 0xffffffff)})
		}
	}
	s.mu.Unlock()
	for _, c := range unlocked {
		s.hub.BroadcastUnlock(c[0], c[1])
	}
	if n > 0 {
		s.metrics.expires.Add(uint64(n))
		s.log.Debug("reaped expired locks", "count", n)
	}
}

// InitAllPixels eagerly creates and funds every pixel account up front (the
// "one million accounts before the first pixel" demo line), so first-touch
// claims never pay an account-creation round-trip.
func (s *Service) InitAllPixels() error {
	if err := s.tb.EnsureAllPixels(s.gridW, s.gridH); err != nil {
		return err
	}
	s.mu.Lock()
	s.allCreated = true
	s.mu.Unlock()
	s.log.Info("eagerly created and funded all pixel accounts", "count", int(s.gridW)*int(s.gridH))
	return nil
}

func (s *Service) ensureSystemPool() {
	s.sysOnce.Do(func() {
		if err := s.tb.EnsureAccounts(); err != nil {
			s.log.Error("failed to create system pool", "err", err)
		}
	})
}

// ensurePixel creates the pixel's account (with the exclusivity flag) and
// funds its single claimable unit. Both steps are idempotent across restarts
// (exists == ok). Accounts are normally provisioned in bulk at boot via
// InitAllPixels; this first-touch path covers cells claimed before that
// finishes or when -eager is off, so a claim never races account creation.
//
// Two concurrent first claims may both pass the created-check before either
// sets s.created; that's benign — EnsureAccounts and Fund are idempotent
// (Fund's deterministic FundID makes a double credit a TransferExists no-op),
// so the pixel still ends up with exactly one claimable unit. Holding s.mu
// across the TB calls was considered and rejected: waiting on network I/O
// under the global lock would stall every claim.
func (s *Service) ensurePixel(x, y uint32) {
	s.mu.Lock()
	all := s.allCreated
	_, ok := s.created[pack(x, y)]
	s.mu.Unlock()
	if all || ok {
		return
	}
	if err := s.tb.EnsureAccounts(tbclient.PixelID(x, y)); err != nil {
		s.log.Error("failed to create pixel account", "x", x, "y", y, "err", err)
		return
	}
	if err := s.tb.Fund(x, y); err != nil {
		s.log.Error("failed to fund pixel", "x", x, "y", y, "err", err)
		return
	}
	s.mu.Lock()
	s.created[pack(x, y)] = struct{}{}
	s.mu.Unlock()
}

// TickMetrics recomputes rolling per-second throughput. Called by the web
// layer's 1s ticker (single goroutine).
func (s *Service) TickMetrics() { s.metrics.tick() }

// MetricsSnapshot returns the dashboard counters: server-side totals, rolling
// throughput, and current lock/pixel counts.
func (s *Service) MetricsSnapshot() map[string]any {
	s.mu.Lock()
	locks := len(s.locks)
	pixels := s.paintedCount()
	s.mu.Unlock()
	return s.metrics.snapshot(locks, pixels)
}

func (s *Service) Describe() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("grid=%dx%d pixels=%d", s.gridW, s.gridH, s.paintedCount())
}
