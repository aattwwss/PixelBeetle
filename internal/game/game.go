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
	mu       sync.Mutex
	gridW    uint32
	gridH    uint32
	pixels   map[uint64]Pixel // key = pack(x,y)
	locks    map[uint64]lock
	claims   map[[16]byte]ClaimMeta // claim transfer id -> meta, until resolved
	byPlayer map[uuid.UUID][16]byte // player -> their single active claim
	created  map[uint64]struct{}    // pixel accounts already ensured in TB
	hub      *hub.Hub
	tb       *tbclient.Client
	log      *slog.Logger
	sysOnce  sync.Once
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
			return [16]byte{}, ErrLockedByOther
		}
		return [16]byte{}, err
	}

	// Void the superseded pending transfer. If this fails the transfer still
	// self-expires in TB after its timeout; the UI is already consistent.
	if hadOld {
		if err := s.tb.Submit(tbclient.BuildVoid(u128(old.Transfer))); err != nil {
			s.log.Warn("failed to void superseded claim", "err", err)
		} else {
			s.hub.PixelUnlocked(old.X, old.Y)
		}
	}

	s.hub.PixelLocked(x, y)
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
		return err
	}

	s.mu.Lock()
	key := pack(meta.X, meta.Y)
	prev := s.pixels[key]
	s.pixels[key] = Pixel{Color: meta.Color, Version: prev.Version + 1}
	s.vacate(claimID, key)
	s.mu.Unlock()

	s.hub.PixelPainted(meta.X, meta.Y, meta.Color)
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
	s.mu.Lock()
	s.vacate(claimID, pack(meta.X, meta.Y))
	s.mu.Unlock()
	s.hub.PixelUnlocked(meta.X, meta.Y)
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

// Grid returns the canvas dimensions.
func (s *Service) Grid() (uint32, uint32) { return s.gridW, s.gridH }

// WarmCache rebuilds the pixel cache from TigerBeetle transfer history so a
// restarted server shows the canvas instead of a blank grid.
func (s *Service) WarmCache() error {
	pixels, err := warm.Scan(s.tb, s.gridW, s.gridH, s.log)
	if err != nil {
		return err
	}
	s.mu.Lock()
	for _, p := range pixels {
		s.pixels[pack(p.X, p.Y)] = Pixel{Color: p.Color, Version: p.Version}
	}
	s.mu.Unlock()
	s.log.Info("warmed pixel cache", "count", len(pixels))
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
	key := pack(ev.X, ev.Y)
	s.mu.Lock()
	cur, ok := s.pixels[key]
	if ok && cur.Color == ev.Color {
		s.mu.Unlock()
		return
	}
	s.pixels[key] = Pixel{Color: ev.Color, Version: cur.Version + 1}
	s.mu.Unlock()
	s.hub.PixelPainted(ev.X, ev.Y, ev.Color)
}

// ReapExpired drops stale in-memory locks. TigerBeetle auto-expires the
// underlying pending transfers (emitting two_phase_expired over CDC), so this
// only unblocks UI claiming — no DB work required.
func (s *Service) ReapExpired() {
	now := time.Now()
	n := 0
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
		}
	}
	s.mu.Unlock()
	if n > 0 {
		s.log.Debug("reaped expired locks", "count", n)
	}
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
	key := pack(x, y)
	s.mu.Lock()
	_, ok := s.created[key]
	s.mu.Unlock()
	if ok {
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
	s.created[key] = struct{}{}
	s.mu.Unlock()
}

func (s *Service) Describe() string {
	return fmt.Sprintf("grid=%dx%d pixels=%d", s.gridW, s.gridH, len(s.Snapshot()))
}

func u128(b [16]byte) tb.Uint128 { return tb.BytesToUint128(b) }
