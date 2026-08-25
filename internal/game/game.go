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
	"pixelbeetle/internal/tbclient"
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
	mu      sync.Mutex
	gridW   uint32
	gridH   uint32
	pixels  map[uint64]Pixel // key = pack(x,y)
	locks   map[uint64]lock
	claims  map[[16]byte]ClaimMeta // claim transfer id -> meta, until resolved
	hub     *hub.Hub
	tb      *tbclient.Client
	log     *slog.Logger
	sysOnce sync.Once
}

func New(w, h uint32, tb *tbclient.Client, h2 *hub.Hub, log *slog.Logger) *Service {
	return &Service{
		gridW:  w,
		gridH:  h,
		pixels: make(map[uint64]Pixel),
		locks:  make(map[uint64]lock),
		claims: make(map[[16]byte]ClaimMeta),
		hub:    h2,
		tb:     tb,
		log:    log,
	}
}

func pack(x, y uint32) uint64 { return uint64(x)<<32 | uint64(y) }

// Claim attempts to lock (x,y) for player. Durable state lands in TigerBeetle
// as a pending transfer; the lock table only gates concurrent attempts.
func (s *Service) Claim(player uuid.UUID, x, y uint32, color uint8) ([16]byte, error) {
	s.ensureSystemPool()

	key := pack(x, y)
	s.mu.Lock()
	if l, ok := s.locks[key]; ok && time.Now().Before(l.expires) && l.player != player {
		s.mu.Unlock()
		return [16]byte{}, ErrLockedByOther
	}
	t := tbclient.NewClaim(x, y, color, player)
	id := t.ID.Bytes()
	s.locks[key] = lock{player: player, expires: time.Now().Add(time.Duration(tbclient.ClaimTimeoutSeconds) * time.Second)}
	s.claims[id] = ClaimMeta{X: x, Y: y, Color: color, Player: player, Transfer: t.ID.Bytes()}
	s.mu.Unlock()

	if err := s.tb.Submit(t); err != nil {
		s.mu.Lock()
		delete(s.locks, key)
		delete(s.claims, id)
		s.mu.Unlock()
		return [16]byte{}, err
	}

	s.hub.PixelLocked(x, y)
	return id, nil
}

// Confirm posts the pending transfer, painting the pixel permanently.
func (s *Service) Confirm(player uuid.UUID, claimID [16]byte) error {
	meta, err := s.resolve(player, claimID)
	if err != nil {
		return err
	}
	post := tbclient.BuildPost(u128(meta.Transfer))
	if err := s.tb.Submit(post); err != nil {
		return err
	}

	s.mu.Lock()
	key := pack(meta.X, meta.Y)
	prev := s.pixels[key]
	s.pixels[key] = Pixel{Color: meta.Color, Version: prev.Version + 1}
	delete(s.claims, claimID)
	delete(s.locks, key)
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
	delete(s.locks, pack(meta.X, meta.Y))
	delete(s.claims, claimID)
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

// ReapExpired drops stale in-memory locks. TigerBeetle auto-expires the
// underlying pending transfers (emitting two_phase_expired over CDC), so this
// only unblocks UI claiming — no DB work required.
func (s *Service) ReapExpired() {
	now := time.Now()
	n := 0
	s.mu.Lock()
	for k, l := range s.locks {
		if now.After(l.expires) {
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

func (s *Service) Describe() string {
	return fmt.Sprintf("grid=%dx%d pixels=%d", s.gridW, s.gridH, len(s.Snapshot()))
}

func u128(b [16]byte) tb.Uint128 { return tb.BytesToUint128(b) }
