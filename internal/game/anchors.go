package game

// The time-travel anchor grid: a checkpoint bitmap of the canvas every
// anchorIntervalMs of event time, so the history view can render "the canvas
// at timestamp T" with one bounded TigerBeetle query instead of folding the
// whole ledger.
//
// Why anchors are unavoidable: TigerBeetle stores events, not state — the
// canvas at T is the fold of ALL posted claims with Timestamp <= T. Without
// an anchor that fold is O(entire history), i.e. ~24s on a 10M-transfer
// ledger. With an anchor at most one minute old, a seek folds at most one
// minute of events (~10–20ms).
//
// Memory model (guarded by Service.mu like the rest of the Service):
//   - list:  one 16-byte entry per minute boundary, dense (idle minutes
//     reference the previous bitmap, so gaps never appear)
//   - pool:  hash-deduplicated checkpoint bitmaps — idle periods collapse to
//     a single shared slice; only minutes that actually changed art allocate
//
// Retention is bounded two ways: at most anchorMax entries, and at most
// maxAnchorPoolBytes of pooled bitmaps (whichever binds first). Oldest
// anchors are evicted to the sidecar file (via onEvict) and remembered as
// lightweight refs, so a seek older than the RAM window still starts from a
// real checkpoint instead of folding the whole ledger.

const (
	anchorIntervalMs  int64 = 60_000 // checkpoint every minute of event time
	anchorMax         int   = 1440   // 24h of minutes
	maxAnchorPoolSize int   = 192 << 20
)

// anchor is one checkpoint: the canvas bitmap exactly as it stood after every
// event with Timestamp < TsMs*1e6 (ns). Hash keys the shared bitmap in pool.
type anchor struct {
	TsMs int64
	Hash uint64
}

// anchorRef is a checkpoint evicted from RAM: Hash identifies the state,
// Off/Len locate its bytes in the append-only sidecar file. Identical
// consecutive states share one blob.
type anchorRef struct {
	TsMs int64
	Off  int64
	Len  uint32
	Hash uint64
}

// onEvictFn, when set, receives every bitmap the grid drops from RAM and
// must persist it, returning the blob's offset/length. An error means the
// checkpoint is dropped entirely (a seek to that era then falls back to a
// full-ledger fold). Nil hook (tests, sidecar disabled) also drops.
type onEvictFn func(tsMs int64, hash uint64, bmp []byte) (off int64, ln uint32, err error)

// anchorGrid owns the checkpoint list and the deduplicated bitmap pool.
type anchorGrid struct {
	list         []anchor
	pool         map[uint64][]byte
	poolBytes    int
	nextBoundary int64       // next minute boundary (ms) to checkpoint; 0 = not started
	evicted      []anchorRef // ascending TsMs; RAM-resident index into the sidecar
	onEvict      onEvictFn
}

// bmpHash is FNV-1a over the bitmap bytes: cheap (one pass, no alloc) and
// collision odds are negligible for this use.
func bmpHash(b []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h
}

// syncTo advances the checkpoint grid across every minute boundary at or
// before eventTsNs, snapshotting curBmp at each boundary. Call it BEFORE
// applying the event to curBmp, so a boundary bitmap never contains the
// event that triggered it. The first call initializes the grid at the
// minute of the first event seen.
func (g *anchorGrid) syncTo(eventTsNs int64, curBmp []byte) {
	eventMs := eventTsNs / 1_000_000
	if g.nextBoundary == 0 {
		g.nextBoundary = (eventMs / anchorIntervalMs) * anchorIntervalMs
	}
	for eventMs >= g.nextBoundary {
		g.insert(g.nextBoundary, curBmp)
		g.nextBoundary += anchorIntervalMs
	}
}

// fillTo inserts boundaries for every minute from the grid's current
// position up to ms (inclusive), snapshotting the UNCHANGED curBmp — used
// after warmup so quiet stretches of the timeline are still dense with
// checkpoints (syncTo alone only fills minutes up to each transfer; a seek
// into a long-quiet era would otherwise fold from an old anchor across
// hours of activity). The duplicated bitmaps dedupe in the pool.
func (g *anchorGrid) fillTo(ms int64, curBmp []byte) {
	if g.nextBoundary == 0 {
		return // no anchors yet: an empty canvas needs no checkpoints
	}
	for g.nextBoundary <= ms {
		g.insert(g.nextBoundary, curBmp)
		g.nextBoundary += anchorIntervalMs
	}
}

// insert records a checkpoint (deduplicated by content hash) and enforces
// the retention caps.
func (g *anchorGrid) insert(tsMs int64, curBmp []byte) {
	h := bmpHash(curBmp)
	if _, ok := g.pool[h]; !ok {
		cp := make([]byte, len(curBmp))
		copy(cp, curBmp)
		if g.pool == nil {
			g.pool = make(map[uint64][]byte)
		}
		g.pool[h] = cp
		g.poolBytes += len(cp)
	}
	g.list = append(g.list, anchor{TsMs: tsMs, Hash: h})
	g.evict()
}

// evict drops the oldest anchors until both retention caps hold. Dropped
// checkpoints are persisted through onEvict (when set) and kept as sidecar
// refs; pool bitmaps nothing references anymore are freed.
func (g *anchorGrid) evict() {
	drop := 0
	for len(g.list) > anchorMax || (g.poolBytes > maxAnchorPoolSize && len(g.list) > 1) {
		drop++
		if drop > len(g.list)-1 { // never evict everything
			drop = len(g.list) - 1
			break
		}
	}
	if drop == 0 {
		return
	}
	dropped := g.list[:drop]
	g.list = g.list[drop:]
	// Recount references: rehashing survivors is cheaper than tracking
	// refcounts through every insert.
	referenced := make(map[uint64]struct{}, len(g.list))
	for _, a := range g.list {
		referenced[a.Hash] = struct{}{}
	}
	for _, a := range dropped {
		if g.onEvict == nil {
			continue // no sidecar: the checkpoint is simply dropped
		}
		// Every dropped boundary gets its own record so the rebooted index
		// keeps the exact timestamp; identical states share a blob via a
		// len==0 reuse record (see writeAnchorBlob).
		off, ln, err := g.onEvict(a.TsMs, a.Hash, g.pool[a.Hash])
		if err != nil {
			continue // persistence failed; the checkpoint is dropped
		}
		g.evicted = append(g.evicted, anchorRef{TsMs: a.TsMs, Off: off, Len: ln, Hash: a.Hash})
	}
	for h, b := range g.pool {
		if _, ok := referenced[h]; !ok {
			g.poolBytes -= len(b)
			delete(g.pool, h)
		}
	}
}

// sidecarAt returns the newest evicted checkpoint at or before tsMs. A
// linear scan (not binary search) is deliberate: the file is append-only
// and may contain a few out-of-order records if two processes ever wrote
// concurrently despite the flock guard — a linear scan is immune to that.
// The index is tiny (a few tens of thousands of 32-byte entries), so the
// scan costs microseconds.
func (g *anchorGrid) sidecarAt(tsMs int64) (anchorRef, bool) {
	var best anchorRef
	found := false
	for _, ref := range g.evicted {
		if ref.TsMs <= tsMs && (!found || ref.TsMs > best.TsMs) {
			best = ref
			found = true
		}
	}
	return best, found
}

// atOrBefore returns the newest checkpoint at or before tsMs. The returned
// slice is SHARED with the pool — callers must copy before mutating. ok is
// false when tsMs predates every retained checkpoint (the caller then folds
// from an empty canvas).
func (g *anchorGrid) atOrBefore(tsMs int64) ([]byte, bool) {
	bmp, _, ok := g.checkpoint(tsMs)
	return bmp, ok
}

// checkpoint is atOrBefore plus the checkpoint's ns boundary: the bitmap
// covers every event strictly before startNs, so the delta query for a frame
// at T begins exactly there (QueryCanvasTransfers is TimestampMin-inclusive).
func (g *anchorGrid) checkpoint(tsMs int64) (bmp []byte, startNs uint64, ok bool) {
	// list is ascending by construction (syncTo appends increasing bounds).
	lo, hi := 0, len(g.list)
	for lo < hi {
		mid := (lo + hi) >> 1
		if g.list[mid].TsMs <= tsMs {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return nil, 0, false
	}
	a := g.list[lo-1]
	b, ok := g.pool[a.Hash]
	return b, uint64(a.TsMs) * 1_000_000, ok
}
