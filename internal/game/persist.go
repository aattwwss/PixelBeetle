package game

// On-disk persistence: the state snapshot (SaveSnapshot/loadSnapshotFile) and
// the anchor sidecar file (writeAnchorBlob/readAnchorBlobLocked), both plain
// self-describing binary formats. See the snapshot format below; the sidecar
// record format is documented on anchorRecHeader.

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// On-disk snapshot of the derived canvas state, so a restart is O(delta since
// last snapshot) instead of O(all history).
//
// Format (little-endian):
//
//	[0:8]    magic "PBSSNAP2"
//	[8:16]   warmTs u64 — max transfer ts folded into this snapshot (the watermark)
//	[16:20]  gridW u32
//	[20:24]  gridH u32
//	[24:32]  firstPaintMs i64
//	[32:40]  lastPaintMs i64
//	[40:48]  bitmap length u64 (= gridW*gridH)
//	[48:...] bitmap bytes (0 = empty, 1..16 = color+1)
//	u64      anchor count
//	n ×      { tsMs i64, hash u64 }                      (ascending tsMs)
//	u64      pool count
//	n ×      { hash u64, len u32, len bytes }            (deduplicated bitmaps)
//
// The pixel map is derived from the bitmap at load (color = byte-1). Locks
// are NOT stored: pending transfers self-expire in TigerBeetle after their
// 3s timeout, so a restarted server correctly starts with none. The anchor
// grid IS stored so history seeks work immediately after a restart without
// re-folding the whole ledger; retention caps (anchors.go) keep its size
// bounded.
const snapshotMagic = "PBSSNAP2"

const snapshotHeaderSize = 8 + 8 + 4 + 4 + 8 + 8 + 8

// SaveSnapshot atomically writes the current canvas/anchor/watermark state to
// path (temp file + fsync + rename, so a crash mid-write never corrupts the
// last good snapshot). A process whose warmup has not completed holds partial
// state and refuses to save — a CDC-fed -warmup=false instance would
// otherwise overwrite a good snapshot with an empty one.
//
// The lock is held only to copy the small fields (bitmap, anchor list, pool
// index) so the snapshot reflects one consistent instant. Pooled bitmaps are
// immutable once inserted, so they are shared by reference — nothing close to
// their full ~192MB (1000×1000 cap) size is copied. Serialization then streams
// section-by-section to the temp file outside the lock, so peak memory stays
// at the bufio buffer, never a whole-payload buffer.
func (s *Service) SaveSnapshot(path string) error {
	s.mu.Lock()
	if !s.warmed {
		s.mu.Unlock()
		return fmt.Errorf("snapshot: save skipped, warmup has not completed " +
			"(this process may hold partial state; refusing to overwrite " +
			"a snapshot that may be good)")
	}
	bmp := make([]byte, len(s.bmp))
	copy(bmp, s.bmp)
	wm := s.warmTs
	fp, lp := s.firstPaintMs, s.lastPaintMs
	anchors := make([]anchor, len(s.ag.list))
	copy(anchors, s.ag.list)
	pool := make(map[uint64][]byte, len(s.ag.pool))
	for h, b := range s.ag.pool {
		pool[h] = b // pooled bitmaps are immutable once inserted; share them
	}
	poolBytes := s.ag.poolBytes
	s.mu.Unlock()

	// Cross-process exclusion: a second live instance (say, a `go run` server
	// that a restart script failed to kill) must not interleave its own
	// temp-write+rename with ours. The sidecar already guards its appends with
	// a flock; the snapshot gets its own lock file (never renamed, so the lock
	// inode is stable). The fd stays open across the rename below, so the lock
	// is held for the whole save and released on return.
	lockPath := path + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("snapshot: lock open: %w", err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("snapshot: flock: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".snapshot-*")
	if err != nil {
		return fmt.Errorf("snapshot: temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	// Stream the payload section-by-section (same byte layout as the format
	// above) instead of materializing it in one buffer: on a 1M-pixel grid the
	// pool section alone is ~192MB, and building it as one []byte would double
	// that peak and churn reallocations per pool entry.
	w := bufio.NewWriterSize(tmp, 1<<20)
	var scratch [8]byte
	writeU64 := func(v uint64) {
		binary.LittleEndian.PutUint64(scratch[:], v)
		w.Write(scratch[:]) // bufio sticks the first error; checked at Flush
	}
	writeU32 := func(v uint32) {
		binary.LittleEndian.PutUint32(scratch[:4], v)
		w.Write(scratch[:4])
	}

	w.Write([]byte(snapshotMagic))
	writeU64(wm)
	writeU32(s.gridW)
	writeU32(s.gridH)
	writeU64(uint64(fp))
	writeU64(uint64(lp))
	writeU64(uint64(len(bmp)))
	w.Write(bmp)
	writeU64(uint64(len(anchors)))
	for _, a := range anchors {
		writeU64(uint64(a.TsMs))
		writeU64(a.Hash)
	}
	writeU64(uint64(len(pool)))
	for h, pb := range pool {
		writeU64(h)
		writeU32(uint32(len(pb)))
		w.Write(pb)
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return fmt.Errorf("snapshot: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("snapshot: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("snapshot: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("snapshot: rename: %w", err)
	}
	s.log.Info("snapshot saved", "path", path, "anchors", len(anchors),
		"poolBitmaps", len(pool), "poolBytes", poolBytes)
	return nil
}

// loadSnapshotFile reads a snapshot written by SaveSnapshot. Any mismatch
// (wrong grid, bad magic, truncated/malformed file) is an error — callers
// fall back to a full replay.
func (s *Service) loadSnapshotFile(path string) (bmp []byte, ag anchorGrid, wm uint64, firstPaint, lastPaint int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ag, 0, 0, 0, err
	}
	defer f.Close()

	hdr := make([]byte, snapshotHeaderSize)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return nil, ag, 0, 0, 0, fmt.Errorf("snapshot: header: %w", err)
	}
	if string(hdr[:8]) != snapshotMagic {
		return nil, ag, 0, 0, 0, fmt.Errorf("snapshot: bad magic %q (old format? delete the file to force a full replay)", hdr[:8])
	}
	wm = binary.LittleEndian.Uint64(hdr[8:16])
	gw := binary.LittleEndian.Uint32(hdr[16:20])
	gh := binary.LittleEndian.Uint32(hdr[20:24])
	firstPaint = int64(binary.LittleEndian.Uint64(hdr[24:32]))
	lastPaint = int64(binary.LittleEndian.Uint64(hdr[32:40]))
	if gw != s.gridW || gh != s.gridH {
		return nil, ag, 0, 0, 0, fmt.Errorf("snapshot: grid %dx%d != server %dx%d", gw, gh, s.gridW, s.gridH)
	}
	bmpLen := binary.LittleEndian.Uint64(hdr[40:48])
	if bmpLen != uint64(gw)*uint64(gh) {
		return nil, ag, 0, 0, 0, fmt.Errorf("snapshot: bitmap length %d != W*H %d", bmpLen, uint64(gw)*uint64(gh))
	}
	bmp = make([]byte, bmpLen)
	if _, err := io.ReadFull(f, bmp); err != nil {
		return nil, ag, 0, 0, 0, fmt.Errorf("snapshot: bitmap: %w", err)
	}

	readU64 := func() (uint64, error) {
		var b [8]byte
		if _, err := io.ReadFull(f, b[:]); err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint64(b[:]), nil
	}
	anchorCount, err := readU64()
	if err != nil {
		return nil, ag, 0, 0, 0, fmt.Errorf("snapshot: anchor count: %w", err)
	}
	ag.list = make([]anchor, 0, min(int(anchorCount), anchorMax))
	for i := uint64(0); i < anchorCount; i++ {
		tsU, err := readU64()
		if err != nil {
			return nil, ag, 0, 0, 0, fmt.Errorf("snapshot: anchor %d: %w", i, err)
		}
		h, err := readU64()
		if err != nil {
			return nil, ag, 0, 0, 0, fmt.Errorf("snapshot: anchor %d: %w", i, err)
		}
		ag.list = append(ag.list, anchor{TsMs: int64(tsU), Hash: h})
	}
	poolCount, err := readU64()
	if err != nil {
		return nil, ag, 0, 0, 0, fmt.Errorf("snapshot: pool count: %w", err)
	}
	ag.pool = make(map[uint64][]byte, min(int(poolCount), anchorMax))
	for i := uint64(0); i < poolCount; i++ {
		h, err := readU64()
		if err != nil {
			return nil, ag, 0, 0, 0, fmt.Errorf("snapshot: pool %d: %w", i, err)
		}
		lenBuf := make([]byte, 4)
		if _, err := io.ReadFull(f, lenBuf); err != nil {
			return nil, ag, 0, 0, 0, fmt.Errorf("snapshot: pool %d: %w", i, err)
		}
		pb := make([]byte, binary.LittleEndian.Uint32(lenBuf))
		if _, err := io.ReadFull(f, pb); err != nil {
			return nil, ag, 0, 0, 0, fmt.Errorf("snapshot: pool %d: %w", i, err)
		}
		ag.pool[h] = pb
		ag.poolBytes += len(pb)
	}
	// Reject trailing garbage / truncation beyond our exact size.
	if extra, _ := io.ReadAll(f); len(extra) != 0 {
		return nil, ag, 0, 0, 0, fmt.Errorf("snapshot: %d trailing bytes", len(extra))
	}
	// Every anchor must reference a pooled bitmap, or the grid is unusable.
	for _, a := range ag.list {
		if _, ok := ag.pool[a.Hash]; !ok {
			return nil, ag, 0, 0, 0, fmt.Errorf("snapshot: anchor %d references missing bitmap %x", a.TsMs, a.Hash)
		}
	}
	// Resume the anchor grid AFTER the newest restored anchor. (Deriving this
	// from the watermark is wrong when the watermark's minute lags the last
	// anchor; the delta fold then re-inserts an existing boundary.) With no
	// anchors, fall back to the watermark's minute — the first event after a
	// boot re-initializes the grid anyway.
	if n := len(ag.list); n > 0 {
		ag.nextBoundary = (ag.list[n-1].TsMs/anchorIntervalMs + 1) * anchorIntervalMs
	} else {
		ag.nextBoundary = (int64(wm/1_000_000)/anchorIntervalMs + 1) * anchorIntervalMs
	}

	s.log.Info("snapshot loaded", "path", path, "warmTs", wm,
		"anchors", len(ag.list), "poolBitmaps", len(ag.pool))
	return bmp, ag, wm, firstPaint, lastPaint, nil
}

// warmFromSnapshot fast-path: boot from the on-disk snapshot and fold only
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

// lockAnchorFile grabs the single-writer flock on a fresh sidecar handle,
// retrying briefly across the restart window. On restart the old server's
// listener is already gone but its final evictions may still hold the flock;
// failing fast there would drop those records and tear permanent holes in
// the append-only sidecar — holes that surface later as bogus timeline
// data (an idle stretch that suddenly looks repainted).
func lockAnchorFile(f *os.File) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
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
		// interleave records and corrupt the index (tsMs inversions). Retry
		// across the restart window instead of failing fast (see
		// lockAnchorFile), then give up without dropping the checkpoint
		// silently.
		if err := lockAnchorFile(f); err != nil {
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

// readAnchorBlobLocked fetches one evicted checkpoint's bytes from the sidecar
// by reference, validating the stored hash. FrameAt calls it while holding
// s.mu; the flock on the handle still guards cross-process access.
func (s *Service) readAnchorBlobLocked(ref anchorRef) ([]byte, error) {
	if s.anchorFile == nil && s.anchorPath != "" {
		f2, err := os.OpenFile(s.anchorPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		if err := lockAnchorFile(f2); err != nil {
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
