// Package game holds PixelBeetle application logic: the pixel read cache,
// the pending-lock table, and the claim service coordinating TigerBeetle with
// the SSE hub.
package game

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"syscall"
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
	Color uint8
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
	mu             sync.Mutex
	gridW          uint32
	gridH          uint32
	pixels         map[uint64]Pixel // key = pack(x,y)
	bmp            []byte           // standing canvas bitmap, row-major, 0=empty / 1..16 = color+1
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
	warmTs         uint64                   // watermark: last transfer ts folded by WarmCache (ns); CDC events at/below it are history replays
	warmed         bool                     // true once WarmCache completed; only a fully-warmed process may write snapshots
	snapshot       string                   // on-disk snapshot path ("" = full replay every boot)
	hub            *hub.Hub
	tb             *tbclient.Client
	log            *slog.Logger
	sysOnce        sync.Once
	metrics        metrics
}

func New(w, h uint32, tb *tbclient.Client, h2 *hub.Hub, log *slog.Logger) *Service {
	return &Service{
		gridW:    w,
		gridH:    h,
		pixels:   make(map[uint64]Pixel),
		bmp:      make([]byte, int(w)*int(h)),
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

	// Anchor checkpoints must see the canvas BEFORE this paint lands, so a
	// minute-boundary bitmap never contains the paint that crossed it.
	nowNs := time.Now().UnixNano()
	s.mu.Lock()
	s.ag.syncTo(nowNs, s.bmp)
	key := pack(meta.X, meta.Y)
	s.pixels[key] = Pixel{Color: meta.Color}
	s.bmp[int(meta.Y)*int(s.gridW)+int(meta.X)] = meta.Color%16 + 1
	s.notePaint(nowNs / 1_000_000)
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
	const limit = 4000
	for from := fromNs; ; {
		page, err := s.tb.QueryCanvasTransfers(from, limit)
		if err != nil {
			return "", 0, err
		}
		stop := false
		for _, t := range page {
			if t.Timestamp > toNs {
				stop = true
				break
			}
			if x, y, color, ok := warm.PostedClaim(t, s.gridW, s.gridH); ok {
				start[int(y)*int(s.gridW)+int(x)] = byte(color%16 + 1)
			}
		}
		if stop || len(page) < limit {
			break
		}
		from = page[len(page)-1].Timestamp + 1
	}
	return base64.StdEncoding.EncodeToString(start), tsMs, nil
}

// SetSnapshot configures the on-disk materialized-state snapshot for O(delta)
// restarts. Call before WarmCache; the server ticker should periodically call
// SaveSnapshot so the file tracks the live state. A sidecar file next to the
// snapshot (<path>.anchors) holds checkpoint bitmaps evicted from RAM so
// old timeline seeks start from a real state instead of the empty canvas.
func (s *Service) SetSnapshot(path string) {
	s.snapshot = path
	s.anchorPath = path + ".anchors"
	s.ag.onEvict = func(tsMs int64, hash uint64, bmp []byte) (int64, uint32, error) {
		return s.writeAnchorBlob(tsMs, hash, bmp)
	}
}

// anchorRecHeader is one sidecar record's fixed prefix: {tsMs i64, hash u64,
// len u32}, optionally followed by len bitmap bytes. A record with len==0
// means "identical state to the previous record" (idle-minute chains
// collapse to 20 bytes). Records are ascending by tsMs by construction.
const anchorRecHeader = 20

// anchorBlobLoc locates a checkpoint bitmap inside the sidecar file.
type anchorBlobLoc struct {
	Off int64
	Len uint32
}

// writeAnchorBlob appends one evicted checkpoint to the sidecar file and
// returns the blob's offset/length. Every eviction writes a record (so the
// rebooted index keeps each boundary's timestamp); identical states share
// the same blob via a len==0 reuse record, and consecutive identical states
// are 20-byte stamps. Runs under s.mu (runtime eviction sites hold it) and
// during the warmup fold (single-threaded, lock-free). Torn appends (crash
// mid-write) are healed by scanAnchorSidecar at next boot.
func (s *Service) writeAnchorBlob(tsMs int64, hash uint64, bmp []byte) (int64, uint32, error) {
	if s.anchorPath == "" {
		return 0, 0, fmt.Errorf("history: anchor sidecar not configured")
	}
	if s.anchorFile == nil {
		f, err := os.OpenFile(s.anchorPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return 0, 0, err
		}
		// Single-writer guard: two servers appending to the same sidecar
		// interleave records and corrupt the index (tsMs inversions). Fail
		// fast instead.
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			return 0, 0, fmt.Errorf("history: anchor sidecar is locked by another process: %w", err)
		}
		s.anchorFile = f
	}
	if s.anchorBlobs == nil {
		s.anchorBlobs = make(map[uint64]anchorBlobLoc)
	}
	end, err := s.anchorFile.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, 0, err
	}

	sameAsLast := hash == s.lastAnchorHash
	loc, known := s.anchorBlobs[hash]
	rec := make([]byte, 0, anchorRecHeader+len(bmp))
	rec = binary.LittleEndian.AppendUint64(rec, uint64(tsMs))
	rec = binary.LittleEndian.AppendUint64(rec, hash)
	switch {
	case sameAsLast || known:
		// Reuse the existing blob: consecutive identical state, or a state
		// already written earlier in the file. 20 bytes, no bitmap payload.
		rec = binary.LittleEndian.AppendUint32(rec, 0)
	default:
		// New state: full record {header, bitmap}; blob starts after the header.
		rec = binary.LittleEndian.AppendUint32(rec, uint32(len(bmp)))
		rec = append(rec, bmp...)
		loc = anchorBlobLoc{Off: end + anchorRecHeader, Len: uint32(len(bmp))}
		s.anchorBlobs[hash] = loc
	}
	if _, err := s.anchorFile.Write(rec); err != nil {
		return 0, 0, err
	}
	s.lastAnchorHash = hash
	return loc.Off, loc.Len, nil
}

// readAnchorBlob fetches one evicted checkpoint's bytes from the sidecar by
// reference, validating the stored hash. Locking variant for callers that
// don't already hold s.mu.
func (s *Service) readAnchorBlob(ref anchorRef) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readAnchorBlobLocked(ref)
}

// readAnchorBlobLocked is readAnchorBlob without locking — FrameAt calls it
// while holding s.mu. The flock on the handle still guards cross-process
// access.
func (s *Service) readAnchorBlobLocked(ref anchorRef) ([]byte, error) {
	if s.anchorFile == nil && s.anchorPath != "" {
		f2, err := os.OpenFile(s.anchorPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(f2.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f2.Close()
			return nil, fmt.Errorf("history: anchor sidecar is locked by another process: %w", err)
		}
		s.anchorFile = f2
	}
	f := s.anchorFile
	if f == nil {
		return nil, fmt.Errorf("history: anchor sidecar not configured")
	}
	if ref.Len == 0 || ref.Len > uint32(maxAnchorPoolSize) {
		return nil, fmt.Errorf("history: sidecar blob length %d implausible", ref.Len)
	}
	bmp := make([]byte, ref.Len)
	if _, err := f.ReadAt(bmp, ref.Off); err != nil {
		return nil, err
	}
	if h := bmpHash(bmp); h != ref.Hash {
		return nil, fmt.Errorf("history: sidecar blob hash mismatch (torn write?)")
	}
	return bmp, nil
}

// scanAnchorSidecar rebuilds the evicted-checkpoint index by walking the
// sidecar file (used at boot; the index is not persisted — the file is
// self-describing). Reuse records (len==0) resolve their blob via the hash
// registered by an earlier full record. Stops at the first torn/short
// record.
func (s *Service) scanAnchorSidecar() error {
	if s.anchorPath == "" {
		return nil
	}
	data, err := os.ReadFile(s.anchorPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var entries []anchorRef
	blobs := make(map[uint64]anchorBlobLoc)
	for pos := 0; pos+anchorRecHeader <= len(data); {
		tsMs := int64(binary.LittleEndian.Uint64(data[pos : pos+8]))
		h := binary.LittleEndian.Uint64(data[pos+8 : pos+16])
		ln := binary.LittleEndian.Uint32(data[pos+16 : pos+20])
		pos += anchorRecHeader
		var loc anchorBlobLoc
		if ln == 0 {
			// Reuse record: point at the blob this hash registered earlier.
			var ok bool
			loc, ok = blobs[h]
			if !ok {
				break // reuse before any full record: corrupt file
			}
		} else {
			if ln > uint32(maxAnchorPoolSize) || pos+int(ln) > len(data) {
				break // torn tail
			}
			loc = anchorBlobLoc{Off: int64(pos), Len: ln}
			blobs[h] = loc
			pos += int(ln)
		}
		entries = append(entries, anchorRef{TsMs: tsMs, Off: loc.Off, Len: loc.Len, Hash: h})
	}

	s.mu.Lock()
	s.ag.evicted = entries
	s.anchorBlobs = blobs
	s.mu.Unlock()
	s.log.Info("anchor sidecar scanned", "entries", len(entries), "blobs", len(blobs), "bytes", len(data))
	return nil
}

// resetAnchorSidecar truncates the sidecar for a full-replay warmup (which
// regenerates the entire evicted timeline from the ledger).
func (s *Service) resetAnchorSidecar() error {
	if s.anchorPath == "" {
		return nil
	}
	if s.anchorFile != nil {
		s.anchorFile.Close()
		s.anchorFile = nil
	}
	f, err := os.OpenFile(s.anchorPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return fmt.Errorf("history: anchor sidecar is locked by another process: %w", err)
	}
	s.anchorFile = f
	s.lastAnchorHash = 0
	s.anchorBlobs = make(map[uint64]anchorBlobLoc)
	return nil
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
		if used, err := s.warmFromSnapshot(s.snapshot); err == nil {
			_ = used
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
	const limit = 4000
	start := time.Now()
	s.log.Info("warmup starting: replaying canvas history from TigerBeetle")
	seen := make(map[uint64]Pixel)
	foldBmp := make([]byte, int(s.gridW)*int(s.gridH))
	var ag anchorGrid
	ag.onEvict = s.ag.onEvict // persist evicted checkpoints during the fold
	var firstPaint, lastPaint int64
	var from uint64
	var scanned uint64
	var lastTs uint64 // max transfer timestamp folded (the CDC watermark)

	// applyFold folds one ledger transfer into the local build state: advance
	// the anchor grid (every transfer, so boundaries stay dense), then paint
	// posted claims into the bitmap + pixel map + timeline bounds.
	apply := func(t tb.Transfer) {
		ag.syncTo(int64(t.Timestamp), foldBmp)
		x, y, color, ok := warm.PostedClaim(t, s.gridW, s.gridH)
		if !ok {
			return
		}
		key := pack(x, y)
		seen[key] = Pixel{Color: color}
		foldBmp[int(y)*int(s.gridW)+int(x)] = byte(color%16 + 1)
		ms := int64(t.Timestamp / 1_000_000)
		if firstPaint == 0 || ms < firstPaint {
			firstPaint = ms
		}
		if ms > lastPaint {
			lastPaint = ms
		}
	}

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
			apply(t)
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
	s.pixels = seen
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
	s.mu.Unlock()
	s.log.Info("warmup complete", "scanned", scanned, "pixels", len(seen),
		"anchors", len(s.ag.list), "anchorPool", len(s.ag.pool), "elapsed", time.Since(start).Round(time.Millisecond))
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
	// Advance the checkpoint grid before mutating the bitmap, for the same
	// reason as in Confirm: a boundary bitmap must not contain its trigger.
	s.ag.syncTo(int64(ev.Timestamp), s.bmp)
	cur, ok := s.pixels[key]
	if ok && cur.Color == ev.Color {
		s.mu.Unlock()
		return
	}
	s.pixels[key] = Pixel{Color: ev.Color}
	s.bmp[int(ev.Y)*int(s.gridW)+int(ev.X)] = ev.Color%16 + 1
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
