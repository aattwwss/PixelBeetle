package game

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"pixelbeetle/internal/warm"
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
	s.mu.Unlock()

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".snapshot-*")
	if err != nil {
		return fmt.Errorf("snapshot: temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	b := make([]byte, 0, snapshotHeaderSize+len(bmp)+len(anchors)*16+64)
	b = append(b, snapshotMagic...)
	b = binary.LittleEndian.AppendUint64(b, wm)
	b = binary.LittleEndian.AppendUint32(b, s.gridW)
	b = binary.LittleEndian.AppendUint32(b, s.gridH)
	b = binary.LittleEndian.AppendUint64(b, uint64(fp))
	b = binary.LittleEndian.AppendUint64(b, uint64(lp))
	b = binary.LittleEndian.AppendUint64(b, uint64(len(bmp)))
	b = append(b, bmp...)
	b = binary.LittleEndian.AppendUint64(b, uint64(len(anchors)))
	for _, a := range anchors {
		b = binary.LittleEndian.AppendUint64(b, uint64(a.TsMs))
		b = binary.LittleEndian.AppendUint64(b, a.Hash)
	}
	b = binary.LittleEndian.AppendUint64(b, uint64(len(pool)))
	for h, pb := range pool {
		b = binary.LittleEndian.AppendUint64(b, h)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(pb)))
		b = append(b, pb...)
	}
	if _, err := tmp.Write(b); err != nil {
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
		"poolBitmaps", len(pool), "poolBytes", s.ag.poolBytes)
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
// the transfers that happened after it (delta). Returns true when used.
func (s *Service) warmFromSnapshot(path string) (bool, error) {
	bmp, ag, wm, fp, lp, err := s.loadSnapshotFile(path)
	if err != nil {
		return false, err
	}
	ag.onEvict = s.ag.onEvict // runtime evictions keep flowing to the sidecar
	start := time.Now()

	// Derive the pixel map from the loaded bitmap (color = byte-1).
	seen := make(map[uint64]Pixel)
	for i, v := range bmp {
		if v > 0 {
			seen[pack(uint32(i%int(s.gridW)), uint32(i/int(s.gridW)))] = Pixel{Color: v - 1}
		}
	}

	// Delta: everything with timestamp strictly after the snapshot's watermark.
	// QueryCanvasTransfers is TimestampMin-inclusive, so start at wm+1.
	var scanned uint64
	var lastTs uint64 = wm
	from := wm + 1
	for {
		page, err := s.tb.QueryCanvasTransfers(from, 4000)
		if err != nil {
			return false, err
		}
		scanned += uint64(len(page))
		for _, t := range page {
			ag.syncTo(int64(t.Timestamp), bmp)
			if x, y, color, ok := warm.PostedClaim(t, s.gridW, s.gridH); ok {
				seen[pack(x, y)] = Pixel{Color: color}
				bmp[int(y)*int(s.gridW)+int(x)] = byte(color%16 + 1)
				ms := int64(t.Timestamp / 1_000_000)
				if fp == 0 || ms < fp {
					fp = ms
				}
				if ms > lp {
					lp = ms
				}
			}
			lastTs = t.Timestamp
		}
		if len(page) < 4000 {
			break
		}
		from = page[len(page)-1].Timestamp + 1
	}

	s.mu.Lock()
	s.pixels = seen
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
		"pixels", len(seen), "elapsed", time.Since(start).Round(time.Millisecond))
	return true, nil
}
