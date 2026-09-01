package bot

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	tb "github.com/tigerbeetle/tigerbeetle-go"

	"pixelbeetle/internal/tbclient"
)

const (
	paintWorkersDefault = 16
	maxPaintRetries     = 3 // per-pixel retries after a locked/errored claim
	paintBackoffBase    = 3 * time.Second
	paintProgressEvery  = 5 * time.Second
)

// paintJob is one placement moving through the worker pool; attempts counts
// how many claim failures it has already suffered.
type paintJob struct {
	P        Placement
	attempts int
}

// resolvePaintOffset centers the drawing on the grid unless the user placed
// it explicitly, then validates it fits. Shared by paintRun (apply) and
// Preview (render-only) so preview never diverges from what painting does.
func resolvePaintOffset(cfg Config, bp Blueprint) ([2]uint32, error) {
	off := cfg.PaintOffset
	if !cfg.PaintOffsetSet {
		off = [2]uint32{
			uint32((int(cfg.GridW) - bp.W) / 2),
			uint32((int(cfg.GridH) - bp.H) / 2),
		}
	}
	if err := bp.ValidateBounds(cfg.GridW, cfg.GridH, off); err != nil {
		return off, err
	}
	return off, nil
}

// shufflePlacements randomly reorders a placement slice in place — the
// -paint-order=random one-liner, factored so the unit test can prove the
// multiset is preserved.
func shufflePlacements(ps []Placement) {
	rand.Shuffle(len(ps), func(i, j int) { ps[i], ps[j] = ps[j], ps[i] })
}

// paintRun paints a blueprint on the canvas, one pixel per claim→confirm
// cycle (pixel-by-pixel so the history timelapse shows the drawing emerge).
// Semantics settled in draw-plan.md "Phase 1 defaults":
//
//   - offset: centered unless cfg.PaintOffsetSet; validated to fit the grid
//   - order: scanline, or one-time random shuffle
//   - workers pull from a shared queue; RPS (when > 0) is a global cap
//   - on errLocked (or any claim/confirm error) retry up to maxPaintRetries
//     times after the 3s claim-window backoff, then give up on that pixel
//   - runs until every placement is painted (or given up), or ctx ends
func paintRun(ctx context.Context, cfg Config, log *slog.Logger, m *Metrics, bp Blueprint, direct *tbclient.Client, httpc *http.Client) (*Metrics, error) {
	off, err := resolvePaintOffset(cfg, bp)
	if err != nil {
		return m, fmt.Errorf("paint: %w", err)
	}
	ps := make([]Placement, len(bp.Placements))
	for i, p := range bp.Placements {
		ps[i] = Placement{X: p.X + int(off[0]), Y: p.Y + int(off[1]), Color: p.Color}
	}
	if cfg.PaintOrder == "random" {
		shufflePlacements(ps)
	}
	workers := cfg.PaintWorkers
	if workers <= 0 {
		workers = paintWorkersDefault
	}

	// Direct mode has no game server to provision accounts: create AND fund
	// exactly the pixels this blueprint touches. Funding matters — an
	// unfunded pixel makes every claim return exceeds_credits (ErrPixelLocked)
	// until it gets its spendable unit; EnsureAccounts alone does not fund.
	// Fund is idempotent, so re-painting the same area is a no-op.
	if direct != nil {
		ids := make([]tb.Uint128, 0, len(ps))
		for _, p := range ps {
			ids = append(ids, tbclient.PixelID(uint32(p.X), uint32(p.Y)))
		}
		if err := direct.EnsureAccounts(ids...); err != nil {
			return m, fmt.Errorf("paint: ensure pixels: %w", err)
		}
		for _, p := range ps {
			if err := direct.Fund(uint32(p.X), uint32(p.Y)); err != nil {
				return m, fmt.Errorf("paint: fund pixel (%d,%d): %w", p.X, p.Y, err)
			}
		}
	}

	m.Total = uint64(len(ps))
	log.Info("paint mode",
		"blueprint", cfg.BlueprintPath,
		"grid", fmt.Sprintf("%dx%d", cfg.GridW, cfg.GridH),
		"offset", fmt.Sprintf("%d,%d", off[0], off[1]),
		"placements", len(ps), "workers", workers, "order", cfg.PaintOrder)
	if len(ps) == 0 {
		log.Info("paint: blueprint has zero placements") // still a valid no-op
		return m, nil
	}

	jobs := make(chan paintJob, len(ps))
	doneCh := make(chan struct{})
	var closeOnce sync.Once
	// doneCh only — jobs is never closed: retry goroutines select on
	// `jobs <- j` vs `<-doneCh`, and select picks randomly among ready
	// cases, so a send to a closed channel could panic. Once doneCh is
	// closed, a lost retry push just sits in the buffer, unread — harmless.
	remaining := int64(len(ps))

	// Seed the queue with every placement (scanline or shuffled order);
	// parallel workers then drain it, so adjacent pixels land near each
	// other on the canvas while up-to-workers are in flight.
	for _, p := range ps {
		jobs <- paintJob{P: p}
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(player uuid.UUID) {
			defer wg.Done()
			// Each worker is its own identity: its own cookie jar (API mode
			// mints the server-side identity) or its own player UUID (direct).
			hc := httpc
			if direct == nil {
				jar, _ := cookiejar.New(nil)
				hc = &http.Client{Timeout: 5 * time.Second, Jar: jar}
			}
			for {
				var j paintJob
				select {
				case pj, ok := <-jobs:
					if !ok {
						return
					}
					j = pj
				case <-doneCh:
					return
				case <-ctx.Done():
					return
				}
				m.ClaimsStarted.Add(1)
				claimID, err := submitClaim(ctx, hc, direct, cfg.Target, player, uint32(j.P.X), uint32(j.P.Y), j.P.Color)
				if err == nil {
					err = resolveClaim(ctx, hc, direct, cfg.Target, claimID, uint32(j.P.X), uint32(j.P.Y), true)
				}
				if err == nil {
					m.Confirmed.Add(1)
					m.Painted.Add(1)
					if atomic.AddInt64(&remaining, -1) == 0 {
						closeOnce.Do(func() { close(doneCh) })
					}
					continue
				}
				m.recordFailure(err, &m.Errors)
				if j.attempts < maxPaintRetries {
					j.attempts++
					go func(j paintJob) {
						backoff := paintBackoffBase + time.Duration(rand.Int63n(int64(time.Second)))
						select {
						case <-time.After(backoff):
							select {
							case jobs <- j:
							case <-doneCh:
							case <-ctx.Done():
							}
						case <-doneCh:
						case <-ctx.Done():
						}
					}(j)
					continue // not terminal: remaining unchanged
				}
				log.Warn("paint: giving up on pixel",
					"x", j.P.X, "y", j.P.Y, "attempts", j.attempts+1, "err", err)
				if atomic.AddInt64(&remaining, -1) == 0 {
					closeOnce.Do(func() { close(doneCh) })
				}
			}
		}(uuid.Must(uuid.NewV7()))
	}

	// Progress heartbeat: painted=X/Y (Z%) every few seconds — the demo
	// moment. Exits when the run finishes or the context cancels.
	go func() {
		t := time.NewTicker(paintProgressEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-doneCh:
				return
			case <-t.C:
				p := m.Painted.Load()
				pct := "0%"
				if m.Total > 0 {
					pct = fmt.Sprintf("%.0f%%", float64(p)/float64(m.Total)*100)
				}
				log.Info("paint progress", "painted", p, "total", m.Total, "pct", pct)
			}
		}
	}()

	wg.Wait()
	log.Info("paint finished",
		"painted", m.Painted.Load(), "total", m.Total,
		"conflicts", m.LockConflicts.Load(), "errors", m.Errors.Load())
	return m, nil
}
