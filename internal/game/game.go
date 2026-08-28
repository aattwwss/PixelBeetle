// Package game holds Canvas Clash application logic: the pixel read cache,
// the pending-lock table, and the claim service coordinating TigerBeetle with
// the SSE hub.
package game

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	tb "github.com/tigerbeetle/tigerbeetle-go"

	"pixelbeetle/internal/canvas"
	"pixelbeetle/internal/hub"
	"pixelbeetle/internal/replay"
	"pixelbeetle/internal/tbclient"
	"pixelbeetle/internal/warm"
)

var (
	ErrLockedByOther = errors.New("pixel locked by another player")
	ErrUnknownClaim  = errors.New("unknown claim")
)

// Pixel is the derived canvas state served to clients.
type Pixel struct {
	Color   uint8
	Version uint64 // equals the pixel account's credits_posted
}

// PaintEvent is a single posted claim, used by the time-travel slider's
// client-side manifest (GET /history).
type PaintEvent struct {
	TsMs  int64  `json:"ts"`
	X     uint32 `json:"x"`
	Y     uint32 `json:"y"`
	Color uint8  `json:"c"`
}

// lock is an in-memory gate on top of the durable pending transfer in TB.
type lock struct {
	player  uuid.UUID
	expires time.Time
}

type ClaimMeta struct {
	X, Y     uint32
	Color    uint8
	Player   uuid.UUID
	Transfer [16]byte
}

type Service struct {
	mu         sync.Mutex
	gridW      uint32
	gridH      uint32
	pixels     map[uint64]Pixel // key = pack(x,y)
	locks      map[uint64]lock
	claims     map[[16]byte]ClaimMeta // claim transfer id -> meta, until resolved
	byPlayer   map[uuid.UUID][16]byte // player -> their single active claim
	created    map[uint64]struct{}    // pixel accounts already ensured in TB
	allCreated bool                   // true once every pixel account is eagerly created+funded
	history    []PaintEvent           // posted claims, ascending ts — the slider manifest (in-memory)
	warmTs     uint64                 // watermark: last transfer ts folded by WarmCache (ns); CDC events at/below it are history replays
	snapshot   string                 // on-disk snapshot path ("" = full replay every boot)
	hub        *hub.Hub
	tb         *tbclient.Client
	log        *slog.Logger
	sysOnce    sync.Once
	metrics    metrics
}

func New(w, h uint32, tb *tbclient.Client, h2 *hub.Hub, log *slog.Logger) *Service {
	return &Service{
		gridW:    w,
		gridH:    h,
		pixels:   make(map[uint64]Pixel),
		locks:    make(map[uint64]lock),
		claims:   make(map[[16]byte]ClaimMeta),
		byPlayer: make(map[uuid.UUID][16]byte),
		created:  make(map[uint64]struct{}),
		hub:      h2,
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
	s.ensureSystemPool()
	s.ensurePixel(x, y)

	key := pack(x, y)
	s.mu.Lock()
	if l, ok := s.locks[key]; ok && time.Now().Before(l.expires) && l.player != player {
		s.mu.Unlock()
		return [16]byte{}, ErrLockedByOther
	}
	// Supersede any prior claim held by this player.
	var old ClaimMeta
	hadOld := false
	if oldID, ok := s.byPlayer[player]; ok {
		if m, ok := s.claims[oldID]; ok {
			old = m
			hadOld = true
			s.vacate(oldID, pack(old.X, old.Y))
		}
	}
	t := tbclient.NewClaim(x, y, color, player)
	id := t.ID.Bytes()
	s.locks[key] = lock{player: player, expires: time.Now().Add(time.Duration(tbclient.ClaimTimeoutSeconds) * time.Second)}
	s.claims[id] = ClaimMeta{X: x, Y: y, Color: color, Player: player, Transfer: t.ID.Bytes()}
	s.byPlayer[player] = id
	s.mu.Unlock()

	// TigerBeetle is the source of truth: if another player's pending holds
	// the pixel's unit, THIS submit fails with exceeds_credits — no app-level
	// lock decides the winner.
	if err := s.tb.Submit(t); err != nil {
		s.mu.Lock()
		delete(s.locks, key)
		delete(s.claims, id)
		delete(s.byPlayer, player)
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
		if err := s.tb.Submit(tbclient.BuildVoid(u128(old.Transfer))); err != nil {
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
	confirm := tbclient.BuildConfirm(u128(meta.Transfer), meta.X, meta.Y)
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

	s.mu.Lock()
	key := pack(meta.X, meta.Y)
	prev := s.pixels[key]
	s.pixels[key] = Pixel{Color: meta.Color, Version: prev.Version + 1}
	s.history = append(s.history, PaintEvent{TsMs: time.Now().UnixMilli(), X: meta.X, Y: meta.Y, Color: meta.Color})
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
	void := tbclient.BuildVoid(u128(meta.Transfer))
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

// resolve validates ownership and removes the claim from the registry.
func (s *Service) resolve(player uuid.UUID, claimID [16]byte) (ClaimMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.claims[claimID]
	if !ok || m.Player != player {
		return ClaimMeta{}, ErrUnknownClaim
	}
	return m, nil
}

// Snapshot returns current derived pixels for SSR rendering / SSE resync.
func (s *Service) Snapshot() map[uint64]Pixel {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[uint64]Pixel, len(s.pixels))
	for k, v := range s.pixels {
		out[k] = v
	}
	return out
}

// SnapshotBmp returns the full canvas as a base64 packed bitmap (one byte per
// cell; 0 = empty, 1..16 = palette color + 1) plus the currently locked cells.
// It's the SSE connect payload and the SSR initial state.
func (s *Service) SnapshotBmp() (string, [][2]uint32) {
	bmp := canvas.NewBitmap(s.gridW, s.gridH)
	now := time.Now()
	var locks [][2]uint32

	s.mu.Lock()
	for k, p := range s.pixels {
		bmp.Set(uint32(k>>32), uint32(k&0xffffffff), p.Color%16+1)
	}
	for k, l := range s.locks {
		if now.Before(l.expires) {
			locks = append(locks, [2]uint32{uint32(k >> 32), uint32(k & 0xffffffff)})
		}
	}
	s.mu.Unlock()

	return bmp.Base64(), locks
}

// Grid returns the canvas dimensions.
func (s *Service) Grid() (uint32, uint32) { return s.gridW, s.gridH }

// History returns the posted-claim manifest (ascending by timestamp) for the
// time-travel slider. It's kept in memory (built by WarmCache, appended on
// each confirm), so the client fetch is O(1) — no per-request TB scan.
func (s *Service) History() []PaintEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PaintEvent, len(s.history))
	copy(out, s.history)
	return out
}

// HistoryLen returns the current manifest length without copying (the server
// ticker uses it to decide whether a snapshot save is needed).
func (s *Service) HistoryLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.history)
}

// TransferTimeRange returns the earliest and latest canvas transfer timestamps
// in milliseconds since epoch. Used as the time-travel slider's min/max bounds
// so the slider spans the actual data range (not epoch→now, which leaves 99%
// blank). Returns (0,0) when there are no transfers.
func (s *Service) TransferTimeRange() (uint64, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.history) == 0 {
		return 0, 0
	}
	return uint64(s.history[0].TsMs), uint64(s.history[len(s.history)-1].TsMs)
}

// SetSnapshot configures an on-disk materialized-state snapshot for O(delta)
// restarts. Call before WarmCache; the server ticker should periodically call
// SaveSnapshot so the file tracks the live state.
func (s *Service) SetSnapshot(path string) { s.snapshot = path }

// WarmCache rebuilds the pixel cache from TigerBeetle transfer history so a
// restarted server shows the canvas instead of a blank grid.
func (s *Service) WarmCache() error {
	// Fast path: load the on-disk snapshot, then fold only the delta since it.
	if s.snapshot != "" {
		if used, err := s.warmFromSnapshot(s.snapshot); err == nil {
			_ = used
			return nil
		} else {
			s.log.Warn("snapshot unusable, falling back to full replay", "err", err)
		}
	}
	const limit = 4000
	start := time.Now()
	s.log.Info("warmup starting: replaying canvas history from TigerBeetle")
	seen := make(map[uint64]Pixel)
	var history []PaintEvent
	var from uint64
	var scanned uint64
	var lastTs uint64 // max transfer timestamp folded (the CDC watermark)
	for pageIdx := 0; ; pageIdx++ {
		page, err := s.tb.QueryCanvasTransfers(from, limit)
		if err != nil {
			return err
		}
		scanned += uint64(len(page))
		if len(page) > 0 {
			lastTs = page[len(page)-1].Timestamp
		}
		for _, t := range page {
			x, y, color, ok := warm.PostedClaim(t, s.gridW, s.gridH)
			if !ok {
				continue
			}
			key := pack(x, y)
			prev := seen[key]
			seen[key] = Pixel{Color: color, Version: prev.Version + 1}
			history = append(history, PaintEvent{TsMs: int64(t.Timestamp / 1_000_000), X: x, Y: y, Color: color})
		}
		// Progress every ~40k transfers (~10 pages of 4000), so a large
		// ledger (bot runs push millions of transfers) shows the server is
		// alive instead of looking hung.
		if pageIdx > 0 && pageIdx%10 == 0 {
			s.log.Info("warming up", "page", pageIdx, "scanned", scanned, "pixels", len(seen))
		}
		if len(page) < limit {
			break
		}
		from = page[len(page)-1].Timestamp + 1
	}
	s.mu.Lock()
	for k, p := range seen {
		s.pixels[k] = p
	}
	s.history = history
	// Watermark: everything at/below the newest folded transfer is history
	// the CDC stream will re-deliver. ApplyEvent drops those so a backlog
	// replay can't re-broadcast old paints or bloat the slider manifest.
	s.warmTs = lastTs
	s.mu.Unlock()
	s.log.Info("warmup complete", "scanned", scanned, "pixels", len(seen), "elapsed", time.Since(start).Round(time.Millisecond))
	return nil
}

// ApplyEvent ingests a CDC event (replay.Sink). It only reacts to posted
// claims, painting the cell if its color changed — which makes it idempotent
// on the instance that originated the claim (same color ⇒ no-op) and correct
// on a second instance consuming the stream (stale color ⇒ paint + broadcast).
func (s *Service) ApplyEvent(ev replay.Event) {
	if ev.Type != replay.TypePosted {
		return
	}
	s.mu.Lock()
	stale := ev.Timestamp <= s.warmTs
	s.mu.Unlock()
	if stale {
		return // already folded into the cache by WarmCache
	}
	key := pack(ev.X, ev.Y)
	s.mu.Lock()
	cur, ok := s.pixels[key]
	if ok && cur.Color == ev.Color {
		s.mu.Unlock()
		return
	}
	s.pixels[key] = Pixel{Color: ev.Color, Version: cur.Version + 1}
	s.history = append(s.history, PaintEvent{TsMs: int64(ev.Timestamp / 1_000_000), X: ev.X, Y: ev.Y, Color: ev.Color})
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

// ensurePixel creates the pixel's account once per process lifetime.
// TODO(plan §4): fold into the batching layer so first-touch claims ride
// along with concurrent batches instead of paying their own round trip.
// ensurePixel creates the pixel's account (with the exclusivity flag) and
// funds its single claimable unit. Both steps are idempotent across restarts
// (exists == ok).
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
	pixels := len(s.pixels)
	s.mu.Unlock()
	return s.metrics.snapshot(locks, pixels)
}

func (s *Service) Describe() string {
	return fmt.Sprintf("grid=%dx%d pixels=%d", s.gridW, s.gridH, len(s.Snapshot()))
}

func u128(b [16]byte) tb.Uint128 { return tb.BytesToUint128(b) }
