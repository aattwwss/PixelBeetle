package game

// Boot-time cache rebuild from TigerBeetle history (WarmCache,
// warmFromSnapshot) plus the one paging iterator they share with history
// seeks (foldFrom).

import (
	"errors"
	"time"

	tb "github.com/tigerbeetle/tigerbeetle-go"

	"pixelbeetle/internal/tbclient"
)

// queryPageSize is the TigerBeetle page size used by every fold/seek path.
const queryPageSize = 4000

// maxFramePages bounds how many TigerBeetle round-trips one history frame may
// issue, so a client can't hammer the cluster with an unbounded seek.
const maxFramePages = 50

// ErrFrameTooBroad means a history frame needs more than maxFramePages pages
// to fold (no checkpoint near the seek point, or an unusually dense era).
// The web layer should surface it as a 4xx rather than retrying.
var ErrFrameTooBroad = errors.New("history frame too broad; seek a newer time")

// foldFrom pages QueryCanvasTransfers from fromNs (inclusive) through the
// ledger, applying apply to each transfer until it returns stop=true or the
// ledger is exhausted. maxPages > 0 bounds the number of TB round-trips;
// exceeding it returns ErrFrameTooBroad. Returns how many pages were fetched.
func (s *Service) foldFrom(fromNs uint64, maxPages int, apply func(tb.Transfer) (stop bool)) (int, error) {
	pages := 0
	for from := fromNs; ; {
		page, err := s.tb.QueryCanvasTransfers(from, queryPageSize)
		if err != nil {
			return pages, err
		}
		pages++
		for _, t := range page {
			if apply(t) {
				return pages, nil
			}
		}
		if len(page) < queryPageSize {
			return pages, nil
		}
		if maxPages > 0 && pages >= maxPages {
			return pages, ErrFrameTooBroad
		}
		from = page[len(page)-1].Timestamp + 1
	}
}

// WarmTs returns the current CDC watermark (the newest ledger timestamp
// folded). The snapshot ticker uses it as a cheap dirty-check.
func (s *Service) WarmTs() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.warmTs
}

// WarmCache rebuilds the pixel cache from TigerBeetle transfer history so a
// restarted server shows the canvas instead of a blank grid.
func (s *Service) WarmCache() error {
	// Fast path: load the on-disk snapshot, then fold only the delta since it.
	if s.snapshot != "" {
		if err := s.warmFromSnapshot(s.snapshot); err == nil {
			if err := s.scanAnchorSidecar(); err != nil {
				s.log.Warn("anchor sidecar scan failed; old timeline seeks will fold from zero", "err", err)
			}
			return nil
		} else {
			s.log.Warn("snapshot unusable, falling back to full replay", "err", err)
		}
	}
	// Full replay regenerates the whole timeline, including evicted-checkpoint
	// sidecar entries — start that file fresh.
	if err := s.resetAnchorSidecar(); err != nil {
		s.log.Warn("anchor sidecar reset failed; evicted checkpoints will be dropped", "err", err)
	}
	start := time.Now()
	s.log.Info("warmup starting: replaying canvas history from TigerBeetle")
	foldBmp := make([]byte, int(s.gridW)*int(s.gridH))
	var ag anchorGrid
	ag.onEvict = s.ag.onEvict // persist evicted checkpoints during the fold
	var firstPaint, lastPaint int64
	var lastTs uint64 // max transfer timestamp folded (the CDC watermark)
	painted := 0
	applied := uint64(0)
	lastLog := start

	// applyFold folds one ledger transfer into the local build state: advance
	// the anchor grid (every transfer, so boundaries stay dense), then paint
	// posted claims into the bitmap + timeline bounds.
	apply := func(t tb.Transfer) bool {
		ag.syncTo(int64(t.Timestamp), foldBmp)
		if x, y, color, ok := tbclient.IsPostedClaim(t, s.gridW, s.gridH); ok {
			idx := int(y)*int(s.gridW) + int(x)
			if foldBmp[idx] == 0 {
				painted++
			}
			foldBmp[idx] = byte(color%16 + 1)
			ms := int64(t.Timestamp / 1_000_000)
			if firstPaint == 0 || ms < firstPaint {
				firstPaint = ms
			}
			if ms > lastPaint {
				lastPaint = ms
			}
		}
		lastTs = t.Timestamp
		applied++
		// Heartbeat on long warmups (bot runs push millions of transfers) so
		// the server looks alive instead of hung.
		if time.Since(lastLog) > 5*time.Second {
			s.log.Info("warming up", "scanned", applied, "pixels", painted)
			lastLog = time.Now()
		}
		return false
	}

	if _, err := s.foldFrom(0, 0, apply); err != nil {
		return err
	}
	s.mu.Lock()
	s.bmp = foldBmp
	s.ag = ag
	s.firstPaintMs = firstPaint
	s.lastPaintMs = lastPaint
	// Watermark: everything at/below the newest folded transfer is history
	// the CDC stream will re-deliver. ApplyEvent drops those so a backlog
	// replay can't re-broadcast old paints.
	s.warmTs = lastTs
	s.warmed = true
	// Fill the checkpoint grid up to now so quiet stretches after the last
	// transfer are still dense with anchors (see anchorGrid.fillTo).
	s.ag.fillTo(time.Now().UnixMilli(), foldBmp)
	anchors := len(s.ag.list)
	pool := len(s.ag.pool)
	s.mu.Unlock()
	s.log.Info("warmup complete", "scanned", applied, "pixels", painted,
		"anchors", anchors, "anchorPool", pool, "elapsed", time.Since(start).Round(time.Millisecond))
	return nil
}

// warmFromSnapshot fast-path: boot from the on-disk snapshot and fold only
// the transfers that happened after it (delta).
func (s *Service) warmFromSnapshot(path string) error {
	bmp, ag, wm, fp, lp, err := s.loadSnapshotFile(path)
	if err != nil {
		return err
	}
	ag.onEvict = s.ag.onEvict // runtime evictions keep flowing to the sidecar
	start := time.Now()

	// Delta: everything with timestamp strictly after the snapshot's watermark.
	// QueryCanvasTransfers is TimestampMin-inclusive, so start at wm+1.
	var lastTs uint64 = wm
	var scanned uint64
	painted := 0
	apply := func(t tb.Transfer) bool {
		scanned++
		ag.syncTo(int64(t.Timestamp), bmp)
		if x, y, color, ok := tbclient.IsPostedClaim(t, s.gridW, s.gridH); ok {
			idx := int(y)*int(s.gridW) + int(x)
			if bmp[idx] == 0 {
				painted++
			}
			bmp[idx] = byte(color%16 + 1)
			ms := int64(t.Timestamp / 1_000_000)
			if fp == 0 || ms < fp {
				fp = ms
			}
			if ms > lp {
				lp = ms
			}
		}
		lastTs = t.Timestamp
		return false
	}
	if _, err := s.foldFrom(wm+1, 0, apply); err != nil {
		return err
	}

	s.mu.Lock()
	s.bmp = bmp
	s.ag = ag
	s.firstPaintMs = fp
	s.lastPaintMs = lp
	s.warmTs = lastTs
	s.warmed = true
	// Fill the checkpoint grid up to now so quiet stretches after the delta
	// are still dense with anchors (see anchorGrid.fillTo).
	s.ag.fillTo(time.Now().UnixMilli(), bmp)
	s.mu.Unlock()
	s.log.Info("warmup complete (snapshot + delta)", "scanned", scanned,
		"pixels", painted, "elapsed", time.Since(start).Round(time.Millisecond))
	return nil
}
