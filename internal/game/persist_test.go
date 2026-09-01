package game

import (
	"bytes"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

func testSvc() *Service {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Service{
		gridW: 8,
		gridH: 8,
		bmp:   make([]byte, 64),
		log:   log,
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	s := testSvc()
	s.bmp[0] = 3
	s.bmp[63] = 16 // palette color 15
	s.firstPaintMs = 1000
	s.lastPaintMs = 5000
	s.warmTs = 123456
	ag := anchorGrid{
		pool:         map[uint64][]byte{0xabc: {1, 2, 3}},
		poolBytes:    3,
		list:         []anchor{{TsMs: 60_000, Hash: 0xabc}},
		nextBoundary: 120_000,
	}
	s.ag = ag
	s.warmed = true

	path := filepath.Join(t.TempDir(), "snap")
	if err := s.SaveSnapshot(path); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	s2 := testSvc()
	bmp, ag2, wm, fp, lp, err := s2.loadSnapshotFile(path)
	if err != nil {
		t.Fatalf("loadSnapshotFile: %v", err)
	}
	if !bytes.Equal(bmp, s.bmp) {
		t.Fatalf("bitmap mismatch after round-trip")
	}
	if wm != s.warmTs || fp != s.firstPaintMs || lp != s.lastPaintMs {
		t.Fatalf("watermark/timeline mismatch: wm=%d fp=%d lp=%d", wm, fp, lp)
	}
	if len(ag2.list) != 1 || ag2.list[0] != ag.list[0] {
		t.Fatalf("anchor list mismatch: %+v", ag2.list)
	}
	if ag2.poolBytes != ag.poolBytes {
		t.Fatalf("poolBytes %d, want %d", ag2.poolBytes, ag.poolBytes)
	}
	if b2, ok := ag2.pool[0xabc]; !ok || !bytes.Equal(b2, []byte{1, 2, 3}) {
		t.Fatalf("pool bitmap mismatch: %v", b2)
	}
	// The grid must resume AFTER the newest restored anchor boundary.
	if ag2.nextBoundary != ag.nextBoundary {
		t.Fatalf("nextBoundary %d, want %d", ag2.nextBoundary, ag.nextBoundary)
	}
}

func TestSaveSnapshotRefusesUnwarmed(t *testing.T) {
	s := testSvc()
	if err := s.SaveSnapshot(filepath.Join(t.TempDir(), "snap")); err == nil {
		t.Fatal("expected error when warmup has not completed")
	}
}

func TestSnapshotRejectsWrongGrid(t *testing.T) {
	s := testSvc()
	s.bmp[0] = 5
	s.warmed = true
	path := filepath.Join(t.TempDir(), "snap")
	if err := s.SaveSnapshot(path); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	s2 := &Service{gridW: 16, gridH: 16, log: testSvc().log}
	if _, _, _, _, _, err := s2.loadSnapshotFile(path); err == nil {
		t.Fatal("expected grid mismatch error")
	}
}

func TestSnapshotBmpDerivesPainted(t *testing.T) {
	s := testSvc()
	s.bmp[2] = 9
	s.bmp[30] = 1
	s.mu.Lock()
	if n := s.paintedCount(); n != 2 {
		t.Fatalf("paintedCount = %d, want 2", n)
	}
	s.mu.Unlock()
	if snap := s.Snapshot(); len(snap) != 2 {
		t.Fatalf("Snapshot len %d, want 2", len(snap))
	}
}
