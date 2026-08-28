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
//	[0:8]   magic "PBSSNAP1"
//	[8:16]  warmTs u64 — max transfer ts folded into this snapshot (the watermark)
//	[16:20] gridW u32
//	[20:24] gridH u32
//	[24:32] history count u64
//	[32:]   history entries, 17 bytes each: tsMs i64, x u32, y u32, color u8
//
// The bitmap/version map are NOT stored: they are derived by folding history
// in one pass at load (fast, ~100ms for millions of events). Locks are NOT
// stored: pending transfers self-expire in TigerBeetle after their 3s timeout
// and the reaper clears stale UI locks, so a restarted server needs none.
const snapshotMagic = "PBSSNAP1"

// snapshotHeaderSize is magic + warmTs + gridW + gridH + count.
const snapshotHeaderSize = 8 + 8 + 4 + 4 + 8

func paintEventSize() int { return 8 + 4 + 4 + 1 } // tsMs + x + y + color

// SaveSnapshot atomically writes the current pixels/history/watermark to
// path (temp file + rename, so a crash mid-write never corrupts the last
// good snapshot). Callers (server ticker) should only call this when history
// has grown since the last save.
func (s *Service) SaveSnapshot(path string) error {
	s.mu.Lock()
	hist := make([]PaintEvent, len(s.history))
	copy(hist, s.history)
	wm := s.warmTs
	s.mu.Unlock()

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".snapshot-*")
	if err != nil {
		return fmt.Errorf("snapshot: temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	b := make([]byte, 0, snapshotHeaderSize+len(hist)*paintEventSize())
	b = append(b, snapshotMagic...)
	b = binary.LittleEndian.AppendUint64(b, wm)
	b = binary.LittleEndian.AppendUint32(b, s.gridW)
	b = binary.LittleEndian.AppendUint32(b, s.gridH)
	b = binary.LittleEndian.AppendUint64(b, uint64(len(hist)))
	for _, e := range hist {
		b = binary.LittleEndian.AppendUint64(b, uint64(e.TsMs))
		b = binary.LittleEndian.AppendUint32(b, e.X)
		b = binary.LittleEndian.AppendUint32(b, e.Y)
		b = append(b, e.Color)
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
	return nil
}

// loadSnapshotFile reads a snapshot written by SaveSnapshot. It returns the
// derived pixels map (folded from history), the ordered history manifest, and
// the watermark. Any mismatch (wrong grid, bad magic, truncated/malformed
// file) is an error — callers fall back to a full replay.
func (s *Service) loadSnapshotFile(path string) (map[uint64]Pixel, []PaintEvent, uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, err
	}
	defer f.Close()

	hdr := make([]byte, snapshotHeaderSize)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return nil, nil, 0, fmt.Errorf("snapshot: header: %w", err)
	}
	if string(hdr[:8]) != snapshotMagic {
		return nil, nil, 0, fmt.Errorf("snapshot: bad magic %q", hdr[:8])
	}
	wm := binary.LittleEndian.Uint64(hdr[8:16])
	gw := binary.LittleEndian.Uint32(hdr[16:20])
	gh := binary.LittleEndian.Uint32(hdr[20:24])
	if gw != s.gridW || gh != s.gridH {
		return nil, nil, 0, fmt.Errorf("snapshot: grid %dx%d != server %dx%d", gw, gh, s.gridW, s.gridH)
	}
	count := binary.LittleEndian.Uint64(hdr[24:32])
	body := make([]byte, int(count)*paintEventSize())
	if _, err := io.ReadFull(f, body); err != nil {
		return nil, nil, 0, fmt.Errorf("snapshot: body: %w", err)
	}
	// Reject trailing garbage / truncation beyond our exact size.
	if extra, _ := io.ReadAll(f); len(extra) != 0 {
		return nil, nil, 0, fmt.Errorf("snapshot: %d trailing bytes", len(extra))
	}

	pixels := make(map[uint64]Pixel, count)
	history := make([]PaintEvent, 0, count)
	for i := 0; i < int(count); i++ {
		e := body[i*paintEventSize() : (i+1)*paintEventSize()]
		ev := PaintEvent{
			TsMs:  int64(binary.LittleEndian.Uint64(e[0:8])),
			X:     binary.LittleEndian.Uint32(e[8:12]),
			Y:     binary.LittleEndian.Uint32(e[12:16]),
			Color: e[16],
		}
		key := pack(ev.X, ev.Y)
		prev := pixels[key]
		pixels[key] = Pixel{Color: ev.Color, Version: prev.Version + 1}
		history = append(history, ev)
	}
	s.log.Info("snapshot loaded", "path", path, "events", len(history),
		"pixels", len(pixels), "warmTs", wm)
	return pixels, history, wm, nil
}

// warmFromSnapshot fast-path: boot from the on-disk snapshot and fold only
// the transfers that happened after it (delta). Returns true when used.
func (s *Service) warmFromSnapshot(path string) (bool, error) {
	pixels, history, wm, err := s.loadSnapshotFile(path)
	if err != nil {
		return false, err
	}
	start := time.Now()
	seen := pixels // continue version counts from the snapshot's fold
	hist := history
	var scanned uint64

	// Delta: everything with timestamp strictly after the snapshot's watermark.
	// QueryCanvasTransfers is TimestampMin-inclusive, so start at wm+1.
	from := wm + 1
	for {
		page, err := s.tb.QueryCanvasTransfers(from, 4000)
		if err != nil {
			return false, err
		}
		scanned += uint64(len(page))
		for _, t := range page {
			x, y, color, ok := warm.PostedClaim(t, s.gridW, s.gridH)
			if !ok {
				continue
			}
			key := pack(x, y)
			prev := seen[key]
			seen[key] = Pixel{Color: color, Version: prev.Version + 1}
			hist = append(hist, PaintEvent{TsMs: int64(t.Timestamp / 1_000_000), X: x, Y: y, Color: color})
			wm = t.Timestamp
		}
		if len(page) < 4000 {
			break
		}
		from = page[len(page)-1].Timestamp + 1
	}

	s.mu.Lock()
	s.pixels = seen
	s.history = hist
	s.warmTs = wm
	s.mu.Unlock()
	s.log.Info("warmup complete (snapshot + delta)", "scanned", scanned,
		"pixels", len(seen), "elapsed", time.Since(start).Round(time.Millisecond))
	return true, nil
}
