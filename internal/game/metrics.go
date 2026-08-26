package game

import (
	"sync/atomic"
	"time"
)

// metrics tracks live game-server counters for the dashboard (build step 6).
// All counters are atomics so they can be touched from any handler goroutine;
// the rolling throughput fields (prevClaims/prevConfirms/prevTick) are written
// only from the single TickMetrics goroutine, so they need no locking.
type metrics struct {
	claims    atomic.Uint64
	confirms  atomic.Uint64
	cancels   atomic.Uint64
	expires   atomic.Uint64
	conflicts atomic.Uint64
	errors    atomic.Uint64

	prevClaims    uint64
	prevConfirms  uint64
	prevTick      time.Time
	claimsPerSec  atomic.Uint64
	confirmsPerSec atomic.Uint64
}

// tick recomputes the rolling 1-second throughput. Called once per second by
// a single goroutine (Service.TickMetrics), hence no lock on the prev* fields.
func (m *metrics) tick() {
	now := time.Now()
	claims := m.claims.Load()
	confirms := m.confirms.Load()
	if !m.prevTick.IsZero() {
		dt := now.Sub(m.prevTick).Seconds()
		if dt > 0 {
			m.claimsPerSec.Store(uint64(float64(claims-m.prevClaims) / dt))
			m.confirmsPerSec.Store(uint64(float64(confirms-m.prevConfirms) / dt))
		}
	}
	m.prevClaims = claims
	m.prevConfirms = confirms
	m.prevTick = now
}

// snapshot returns the current counter values plus the caller-supplied live
// lock count and painted-pixel count, ready for JSON marshalling into the
// SSE "metrics" signal.
func (m *metrics) snapshot(locks, pixels int) map[string]any {
	return map[string]any{
		"claims":         m.claims.Load(),
		"confirms":       m.confirms.Load(),
		"cancels":        m.cancels.Load(),
		"expires":        m.expires.Load(),
		"conflicts":      m.conflicts.Load(),
		"errors":         m.errors.Load(),
		"claimsPerSec":   m.claimsPerSec.Load(),
		"confirmsPerSec": m.confirmsPerSec.Load(),
		"locks":          locks,
		"pixels":         pixels,
	}
}
