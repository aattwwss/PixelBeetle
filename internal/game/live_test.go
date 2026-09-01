package game

// Live integration tests for the claim/confirm/supersede/reap core (the one
// area with no test coverage — finding #7 in feedback.md). These run against
// a throwaway 3-replica TigerBeetle cluster that TestMain formats, starts,
// and kills for this suite only — the dev cluster on :3000-3002 and its
// ledger are never touched.
//
// Running:
//
//	go test ./internal/game/            // auto-starts its own cluster on :3100-3102
//	PIXELBEETLE_LIVE_TESTS=off go test ./internal/game/  // skips the live suite
//
// The suite self-skips when ./bin/tigerbeetle is missing or ports 3100-3102
// are taken; the pure unit tests (anchors, snapshot round-trip) always run.
//
// Pixels are a shared, permanent resource across the whole ledger, and every
// test paints into ledger 1, so each test claims a disjoint coordinate range
// (the table below) — tests are sequential (no t.Parallel) and must never
// touch another test's pixels.

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"pixelbeetle/internal/hub"
	"pixelbeetle/internal/replay"
	"pixelbeetle/internal/tbclient"
)

// --- Throwaway-cluster lifecycle -------------------------------------------------

var (
	live      *tbclient.Client // shared client into the suite's cluster
	liveReady bool
	liveAddrs []string
	liveProcs []*exec.Cmd
	liveDir   string
)

const liveGRID = 8 // grid used by every live Service (8x8 = 64 pixels)

func TestMain(m *testing.M) {
	startLiveCluster()
	code := m.Run()
	stopLiveCluster()
	os.Exit(code)
}

// startLiveCluster formats and starts its own 3-replica cluster on
// 127.0.0.1:3100-3102 with data files in a temp dir. On any unavailability it
// prints a skip reason and leaves liveReady=false — the live tests then
// skip, unit tests still run.
func startLiveCluster() {
	if os.Getenv("PIXELBEETLE_LIVE_TESTS") == "off" {
		fmt.Println("game/live: skipping (PIXELBEETLE_LIVE_TESTS=off)")
		return
	}
	bin := "../../bin/tigerbeetle"
	if _, err := os.Stat(bin); err != nil {
		fmt.Println("game/live: skipping (no ./bin/tigerbeetle)")
		return
	}
	const base = 3100
	for i := 0; i < 3; i++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
		if err != nil {
			fmt.Printf("game/live: skipping (port %d in use)\n", base+i)
			return
		}
		ln.Close()
	}

	dir, err := os.MkdirTemp("", "pixelbeetle-live-")
	if err != nil {
		fmt.Println("game/live: skipping (temp dir:", err, ")")
		return
	}
	liveDir = dir

	addrs := make([]string, 3)
	for i := 0; i < 3; i++ {
		addrs[i] = fmt.Sprintf("127.0.0.1:%d", base+i)
	}
	// Format the three replica data files.
	for i := 0; i < 3; i++ {
		path := filepath.Join(dir, fmt.Sprintf("replica_%d.tigerbeetle", i))
		cmd := exec.Command(bin, "format",
			"--cluster=0", fmt.Sprintf("--replica=%d", i),
			"--replica-count=3", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("game/live: skipping (format: %v\n%s)\n", err, out)
			stopLiveCluster()
			return
		}
	}
	// Start them.
	for i := 0; i < 3; i++ {
		path := filepath.Join(dir, fmt.Sprintf("replica_%d.tigerbeetle", i))
		logf, err := os.Create(filepath.Join(dir, fmt.Sprintf("replica_%d.log", i)))
		if err != nil {
			fmt.Println("game/live: skipping (log file:", err, ")")
			stopLiveCluster()
			return
		}
		cmd := exec.Command(bin, "start", "--addresses="+strings.Join(addrs, ","), path)
		cmd.Stdout = logf
		cmd.Stderr = logf
		if err := cmd.Start(); err != nil {
			fmt.Println("game/live: skipping (start:", err, ")")
			stopLiveCluster()
			return
		}
		liveProcs = append(liveProcs, cmd)
	}

	// Wait for quorum readiness by polling a client query.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c, err := tbclient.Connect(0, addrs)
		if err == nil {
			if _, qerr := c.QueryCanvasTransfers(0, 1); qerr == nil {
				live = c
				liveAddrs = addrs
				liveReady = true
				fmt.Printf("game/live: throwaway cluster ready on %s\n", strings.Join(addrs, ","))
				return
			}
			c.Close() // query not served yet; try again after a beat
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Println("game/live: skipping (cluster did not become ready in 20s)")
	stopLiveCluster()
}

func stopLiveCluster() {
	for _, p := range liveProcs {
		if p.Process != nil {
			p.Process.Kill()
			p.Wait()
		}
	}
	liveProcs = nil
	if live != nil {
		live.Close()
		live = nil
	}
	if liveDir != "" {
		os.RemoveAll(liveDir)
		liveDir = ""
	}
	liveReady = false
}

// liveSvc is the standard Service factory for live tests: an 8x8 grid on the
// suite's cluster, with a discard logger and a plain hub (broadcasts are
// non-blocking no-ops with no subscribers). Skips when the cluster is down.
func liveSvc(t *testing.T) *Service {
	t.Helper()
	if !liveReady {
		t.Skip("live TigerBeetle cluster unavailable (PIXELBEETLE_LIVE_TESTS=off or binary/ports missing)")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(liveGRID, liveGRID, live, hub.New(log), log)
}

func bmpIdx(x, y uint32) int { return int(y)*liveGRID + int(x) }

func bmpAt(t *testing.T, s *Service, x, y uint32) byte {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bmp[bmpIdx(x, y)]
}

// lockedCells counts live locks in the lock table (exported lanes we need in
// assertions; helper must not deadlock — reads only).
func liveLockCount(t *testing.T, s *Service) int {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.locks)
}

func liveClaimCount(t *testing.T, s *Service) int {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.claims)
}

// --- Claim / confirm / cancel semantics -----------------------------------------

// TestClaimConfirmPaintsPixel: claim posts a pending, confirm posts the paint,
// so the cache bitmap updates and both the lock table and claim registry
// clear. The confirm's linked batch also re-funds the pixel's unit, so
// durability is proven behaviorally: the posted claim leg must exist in the
// TigerBeetle ledger idempotently re-foldable by warmup (IsPostedClaim).
// (Repaintability is by design — exclusivity only holds during the 3s pending
// window, which TestClaimLockedPixelFails and
// TestSupersedeRestoreOnFailedSubmit exercise.)
func TestClaimConfirmPaintsPixel(t *testing.T) {
	s := liveSvc(t)
	p := uuid.Must(uuid.NewV7())

	id, err := s.Claim(p, 0, 0, 2)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if n := liveLockCount(t, s); n != 1 {
		t.Fatalf("expected 1 pending lock, got %d", n)
	}
	if n := liveClaimCount(t, s); n != 1 {
		t.Fatalf("expected 1 pending claim, got %d", n)
	}

	if err := s.Confirm(p, id); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if got := bmpAt(t, s, 0, 0); got != 3 { // color 2, stored as color+1
		t.Fatalf("pixmap at (0,0) = %d, want 3", got)
	}
	if n := liveLockCount(t, s); n != 0 {
		t.Fatalf("locks not cleared after confirm: %d", n)
	}
	if n := liveClaimCount(t, s); n != 0 {
		t.Fatalf("claims not cleared after confirm: %d", n)
	}

	// TB-side proof: the confirm landed durably in the ledger as a posted
	// claim leg for this pixel — exactly what the CDC stream and warmup fold.
	page, err := live.QueryCanvasTransfers(0, queryPageSize)
	if err != nil {
		t.Fatalf("QueryCanvasTransfers: %v", err)
	}
	found := false
	for _, tr := range page {
		x, y, color, ok := tbclient.IsPostedClaim(tr, liveGRID, liveGRID)
		if ok && x == 0 && y == 0 {
			found = true
			if color != 2 {
				t.Fatalf("posted claim color %d, want 2", color)
			}
			break
		}
	}
	if !found {
		t.Fatal("no posted claim leg for (0,0) in the ledger after confirm")
	}
}

// TestClaimLockedPixelFails: while a pending claim holds a pixel, a second
// claim on the same cell fails fast with the locked sentinel and the first
// claim stays confirmable.
func TestClaimLockedPixelFails(t *testing.T) {
	s := liveSvc(t)
	p1 := uuid.Must(uuid.NewV7())
	p2 := uuid.Must(uuid.NewV7())

	id1, err := s.Claim(p1, 1, 1, 2)
	if err != nil {
		t.Fatalf("Claim(p1): %v", err)
	}
	if _, err := s.Claim(p2, 1, 1, 2); !errors.Is(err, ErrLockedByOther) {
		t.Fatalf("second claim on a locked pixel: got %v, want ErrLockedByOther", err)
	}
	if got, _ := s.resolve(p1, id1); got.X != 1 || got.Y != 1 {
		t.Fatalf("p1's claim was disturbed: %+v", got)
	}
	if err := s.Confirm(p1, id1); err != nil {
		t.Fatalf("Confirm(p1) after rejected rival: %v", err)
	}
}

// TestClaimOutOfBounds: coordinates outside the grid are rejected before any
// TigerBeetle interaction (regression for the remote-deadlock finding #1).
func TestClaimOutOfBounds(t *testing.T) {
	s := liveSvc(t)
	if _, err := s.Claim(uuid.Must(uuid.NewV7()), liveGRID, 0, 2); !errors.Is(err, ErrOutOfBounds) {
		t.Fatalf("x==gridW: got %v, want ErrOutOfBounds", err)
	}
	if _, err := s.Claim(uuid.Must(uuid.NewV7()), 0, liveGRID, 2); !errors.Is(err, ErrOutOfBounds) {
		t.Fatalf("y==gridH: got %v, want ErrOutOfBounds", err)
	}
}

// TestSupersedeRestoreOnFailedSubmit is the regression test for finding #5
// (supersede ordering destroyed the old claim): when a new claim races
// another player's pending in TigerBeetle and the submit fails, the player's
// previous claim must be restored exactly and stay confirmable.
//
// The conflict is engineered with TWO Service instances sharing the cluster:
// the rival's lock lives in s1, so s2 (starting from an empty lock table)
// passes its in-memory check and only hits the conflict inside TB — exactly
// the cross-process race the fix targets.
func TestSupersedeRestoreOnFailedSubmit(t *testing.T) {
	s1 := liveSvc(t)
	s2 := liveSvc(t) // same cluster, independent lock table

	rival := uuid.Must(uuid.NewV7()) // holds P_b in TB via s1
	player := uuid.Must(uuid.NewV7())

	if _, err := s1.Claim(rival, 2, 3, 4); err != nil {
		t.Fatalf("rival Claim(P_b): %v", err)
	}
	idA, err := s2.Claim(player, 2, 2, 3) // player's claim on P_a succeeds
	if err != nil {
		t.Fatalf("Claim(P_a): %v", err)
	}

	// Same player supersedes toward P_b — locally fine (s2's lock table is
	// empty), but TigerBeetle rejects it: P_b is held by the rival's pending.
	if _, err := s2.Claim(player, 2, 3, 4); !errors.Is(err, ErrLockedByOther) {
		t.Fatalf("Claim(P_b) over a TB-held pixel: got %v, want ErrLockedByOther", err)
	}

	// The vacated old claim must be back: player index, registry, and lock.
	s2.mu.Lock()
	restored, ok := s2.byPlayer[player]
	_, hasClaim := s2.claims[restored]
	_, hasLock := s2.locks[pack(2, 2)]
	s2.mu.Unlock()
	if !ok || restored != idA || !hasClaim || !hasLock {
		t.Fatalf("old claim not restored after failed supersede: byPlayer=%v restored=%x hasClaim=%v hasLock=%v",
			ok, restored, hasClaim, hasLock)
	}

	// And it must still be durable in TB: confirm and paint.
	if err := s2.Confirm(player, idA); err != nil {
		t.Fatalf("Confirm(restored claim): %v", err)
	}
	if got := bmpAt(t, s2, 2, 2); got != 4 { // color 3 + 1
		t.Fatalf("P_a not painted after restore+confirm: %d", got)
	}
}

// TestCancelVoidsPending: cancel voids the pending in TB immediately, clears
// local state, and returns the cell to claimable by another player — no
// 3-second wait for expiry.
func TestCancelVoidsPending(t *testing.T) {
	s := liveSvc(t)
	p1 := uuid.Must(uuid.NewV7())
	p2 := uuid.Must(uuid.NewV7())

	id, err := s.Claim(p1, 3, 3, 5)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.Cancel(p1, id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if n := liveClaimCount(t, s); n != 0 {
		t.Fatalf("claim registry not cleared after cancel: %d", n)
	}
	if n := liveLockCount(t, s); n != 0 {
		t.Fatalf("lock table not cleared after cancel: %d", n)
	}

	// The void is durable: another player claims the same pixel immediately.
	if _, err := s.Claim(p2, 3, 3, 5); err != nil {
		t.Fatalf("reclaim after cancel: %v", err)
	}
}

// TestReapExpired: staleness in the lock table is judged by the lock's
// expires field; reaping clears lock + claim + player index and is a no-op a
// second time. (The TB pending self-expires after its 3s timeout — the flash
// path is covered by Claim/Cancel above; the reaper only unblocks UI lanes.)
func TestReapExpired(t *testing.T) {
	s := liveSvc(t)
	p := uuid.Must(uuid.NewV7())

	id, err := s.Claim(p, 4, 4, 2)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// Age the lock artificially instead of sleeping past ClaimTimeoutSeconds.
	s.mu.Lock()
	s.locks[pack(4, 4)] = lock{player: p, expires: time.Now().Add(-time.Second)}
	if _, ok := s.claims[id]; !ok {
		s.mu.Unlock()
		t.Fatal("claim should exist before reap")
	}
	s.mu.Unlock()

	s.ReapExpired()
	if n := liveLockCount(t, s); n != 0 {
		t.Fatalf("lock survived reap: %d", n)
	}
	if n := liveClaimCount(t, s); n != 0 {
		t.Fatalf("claim survived reap: %d", n)
	}
	s.mu.Lock()
	_, stillTracked := s.byPlayer[p]
	s.mu.Unlock()
	if stillTracked {
		t.Fatal("byPlayer entry survived reap")
	}
	s.ReapExpired() // second pass must be a harmless no-op
}

// --- CDC replay (ApplyEvent) ------------------------------------------------------

// TestApplyEventDedupeAndPaint: ApplyEvent paints fresh posted events, drops
// redeliveries at/below the watermark (so warmup backlog replay can't
// re-broadcast), and a stale redelivery with a different color cannot regress
// a newer paint — the dedupe the replay sink and watermark rely on.
func TestApplyEventDedupeAndPaint(t *testing.T) {
	s := liveSvc(t)
	p := uuid.Must(uuid.NewV7())

	// Direct paint raises warmTs to the wall-clock proxy; the CDC event for
	// that same paint carries a TB timestamp below it → naturally dropped.
	id, err := s.Claim(p, 5, 0, 3)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.Confirm(p, id); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	wm := s.WarmTs()
	s.ApplyEvent(replay.Event{Type: replay.TypePosted, Timestamp: wm, X: 5, Y: 0, Color: 3})
	if got := bmpAt(t, s, 5, 0); got != 4 {
		t.Fatalf("watermark-redelivery changed the paint: %d", got)
	}

	// A fresh posted event repaints the cell.
	s.ApplyEvent(replay.Event{Type: replay.TypePosted, Timestamp: wm + 1, X: 5, Y: 0, Color: 5})
	if got := bmpAt(t, s, 5, 0); got != 6 {
		t.Fatalf("fresh event did not paint: %d", got)
	}

	// The stale redelivery (older color, at/below watermark) must not regress.
	s.ApplyEvent(replay.Event{Type: replay.TypePosted, Timestamp: wm, X: 5, Y: 0, Color: 3})
	if got := bmpAt(t, s, 5, 0); got != 6 {
		t.Fatalf("stale redelivery regressed a newer paint: %d", got)
	}

	// Non-posted event types are ignored entirely.
	s.ApplyEvent(replay.Event{Type: replay.TypePending, Timestamp: wm + 1, X: 5, Y: 1, Color: 3})
	if got := bmpAt(t, s, 5, 1); got != 0 {
		t.Fatalf("pending event should not paint: %d", got)
	}
}

// --- Warmup / history / snapshot -----------------------------------------------

// TestWarmCacheFromLiveLedger: a fresh Service rebuilt from the ledger (full
// replay — no snapshot) must see every paint a warm-peer made, with the
// correct colors, and come out warmed with a positive watermark.
func TestWarmCacheFromLiveLedger(t *testing.T) {
	s1 := liveSvc(t)
	p := uuid.Must(uuid.NewV7())
	paints := [][3]uint32{{6, 6, 3}, {6, 7, 1}, {0, 7, 5}}
	for _, pc := range paints {
		id, err := s1.Claim(p, pc[0], pc[1], uint8(pc[2]))
		if err != nil {
			t.Fatalf("Claim(%v): %v", pc, err)
		}
		if err := s1.Confirm(p, id); err != nil {
			t.Fatalf("Confirm(%v): %v", pc, err)
		}
	}

	s2 := liveSvc(t)
	if err := s2.WarmCache(); err != nil {
		t.Fatalf("WarmCache: %v", err)
	}
	s2.mu.Lock()
	if !s2.warmed || s2.warmTs == 0 {
		s2.mu.Unlock()
		t.Fatalf("warmup did not complete: warmed=%v warmTs=%d", s2.warmed, s2.warmTs)
	}
	s2.mu.Unlock()
	for _, pc := range paints {
		if got := bmpAt(t, s2, pc[0], pc[1]); got != byte(pc[2]+1) {
			t.Fatalf("warmed pixmap at (%d,%d) = %d, want %d", pc[0], pc[1], got, pc[2]+1)
		}
	}
}

// TestFrameAtLive: history seeks via live ledger folds. A frame at/after the
// newest paint returns the standing bitmap; a frame at the epoch returns an
// empty canvas without erroring (bounded pages — the old unbounded-loop
// finding #13's live check).
func TestFrameAtLive(t *testing.T) {
	s := liveSvc(t)
	p := uuid.Must(uuid.NewV7())

	id, err := s.Claim(p, 5, 5, 4)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.Confirm(p, id); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	s.mu.Lock()
	last := s.lastPaintMs
	s.mu.Unlock()

	b64, eff, err := s.FrameAt(last)
	if err != nil {
		t.Fatalf("FrameAt(newest): %v", err)
	}
	if eff != last {
		t.Fatalf("effective frame %d, want %d", eff, last)
	}
	bmp, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("frame b64 decode: %v", err)
	}
	if bmp[bmpIdx(5, 5)] != 5 { // color 4 + 1
		t.Fatalf("current frame missing the paint: %d", bmp[bmpIdx(5, 5)])
	}

	// Epoch frame: empty canvas, bounded fold, no error.
	b64z, _, err := s.FrameAt(0)
	if err != nil {
		t.Fatalf("FrameAt(0): %v", err)
	}
	bmpz, err := base64.StdEncoding.DecodeString(b64z)
	if err != nil {
		t.Fatalf("epoch frame b64 decode: %v", err)
	}
	for i, v := range bmpz {
		if v != 0 {
			t.Fatalf("epoch frame should be empty, byte %d = %d", i, v)
		}
	}
}

// TestSnapshotRoundTripLive: the real restart path — warm a Service, paint,
// SaveSnapshot, then boot a second Service from that snapshot via
// SetSnapshot+WarmCache (snapshot + delta fold). Both bitmaps must agree
// byte-for-byte, proving warmup via snapshot is lossless against a live
// ledger.
func TestSnapshotRoundTripLive(t *testing.T) {
	s1 := liveSvc(t)
	if err := s1.WarmCache(); err != nil {
		t.Fatalf("WarmCache(s1): %v", err)
	}
	p := uuid.Must(uuid.NewV7())
	for _, pc := range [][3]uint32{{7, 6, 2}, {7, 7, 3}} {
		id, err := s1.Claim(p, pc[0], pc[1], uint8(pc[2]))
		if err != nil {
			t.Fatalf("Claim(%v): %v", pc, err)
		}
		if err := s1.Confirm(p, id); err != nil {
			t.Fatalf("Confirm(%v): %v", pc, err)
		}
	}

	path := filepath.Join(t.TempDir(), "live-snapshot")
	if err := s1.SaveSnapshot(path); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	s1.mu.Lock()
	got := append([]byte(nil), s1.bmp...)
	s1.mu.Unlock()

	s2 := liveSvc(t)
	s2.SetSnapshot(path)
	if err := s2.WarmCache(); err != nil {
		t.Fatalf("WarmCache(s2 from snapshot): %v", err)
	}
	s2.mu.Lock()
	warmBmp := append([]byte(nil), s2.bmp...)
	s2.mu.Unlock()
	if !bytes.Equal(got, warmBmp) {
		t.Fatalf("snapshot boot differs from live state (%d vs %d painted bytes)",
			bytes.Count(got, []byte{0}), bytes.Count(warmBmp, []byte{0}))
	}
}
