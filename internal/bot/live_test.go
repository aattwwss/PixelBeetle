package bot

// Live integration tests for the paint mode: a blueprint drawn through the
// bot's claim→confirm cycle straight into a real TigerBeetle ledger. Uses the
// same throwaway-cluster pattern as internal/game/live_test.go but on ports
// 3200-3202 (the game suite uses 3100-3102; go test runs packages in
// parallel, so the ports must not overlap or one suite would silently skip).
// The dev cluster on :3000-3002 and its ledger are never touched.
//
// Running:
//
//	go test ./internal/bot/            // auto-starts its own cluster on :3200-3202
//	PIXELBEETLE_LIVE_TESTS=off go test ./internal/bot/  // skips the live suite
//
// Self-skips when ./bin/tigerbeetle is missing or the ports are taken; the
// unit tests (palette, blueprint parsing) always run.

import (
	"context"
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

	"pixelbeetle/internal/tbclient"
)

var (
	liveAddrs []string
	liveReady bool
	liveProcs []*exec.Cmd
	liveDir   string
)

func TestMain(m *testing.M) {
	startLiveCluster()
	code := m.Run()
	stopLiveCluster()
	os.Exit(code)
}

func startLiveCluster() {
	if os.Getenv("PIXELBEETLE_LIVE_TESTS") == "off" {
		fmt.Println("bot/live: skipping (PIXELBEETLE_LIVE_TESTS=off)")
		return
	}
	bin := "../../bin/tigerbeetle"
	if _, err := os.Stat(bin); err != nil {
		fmt.Println("bot/live: skipping (no ./bin/tigerbeetle)")
		return
	}
	const base = 3200
	for i := 0; i < 3; i++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
		if err != nil {
			fmt.Printf("bot/live: skipping (port %d in use)\n", base+i)
			return
		}
		ln.Close()
	}

	dir, err := os.MkdirTemp("../../data", "live-bot-") // disk-backed: each replica needs ~1GiB, too big for tmpfs (/tmp)
	if err != nil {
		fmt.Println("bot/live: skipping (temp dir:", err, ")")
		return
	}
	liveDir = dir
	addrs := make([]string, 3)
	for i := range addrs {
		addrs[i] = fmt.Sprintf("127.0.0.1:%d", base+i)
	}
	for i := 0; i < 3; i++ {
		path := filepath.Join(dir, fmt.Sprintf("replica_%d.tigerbeetle", i))
		cmd := exec.Command(bin, "format",
			"--cluster=0", fmt.Sprintf("--replica=%d", i),
			"--replica-count=3", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("bot/live: skipping (format: %v\n%s)\n", err, out)
			stopLiveCluster()
			return
		}
	}
	for i := 0; i < 3; i++ {
		path := filepath.Join(dir, fmt.Sprintf("replica_%d.tigerbeetle", i))
		logf, err := os.Create(filepath.Join(dir, fmt.Sprintf("replica_%d.log", i)))
		if err != nil {
			fmt.Println("bot/live: skipping (log file:", err, ")")
			stopLiveCluster()
			return
		}
		cmd := exec.Command(bin, "start", "--addresses="+strings.Join(addrs, ","), path)
		cmd.Stdout = logf
		cmd.Stderr = logf
		if err := cmd.Start(); err != nil {
			fmt.Println("bot/live: skipping (start:", err, ")")
			stopLiveCluster()
			return
		}
		liveProcs = append(liveProcs, cmd)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c, err := tbclient.Connect(0, addrs)
		if err == nil {
			if _, qerr := c.QueryCanvasTransfers(0, 1); qerr == nil {
				c.Close()
				liveAddrs = addrs
				liveReady = true
				fmt.Printf("bot/live: throwaway cluster ready on %s\n", strings.Join(addrs, ","))
				return
			}
			c.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Println("bot/live: skipping (cluster did not become ready in 20s)")
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
	if liveDir != "" {
		os.RemoveAll(liveDir)
		liveDir = ""
	}
	liveReady = false
}

const (
	paintGrid = 16 // direct-mode grid for the live painter tests
	paintArt  = `legend: k=#222222 w=#ffffff
kw
wk`
)

// painterConfig is the direct-mode paint config for the live suite; the
// blueprint file lives in the test's temp dir (BlueprintPath is a path).
func painterConfig(t *testing.T) Config {
	t.Helper()
	if !liveReady {
		t.Skip("live TigerBeetle cluster unavailable (PIXELBEETLE_LIVE_TESTS=off or binary/ports missing)")
	}
	path := filepath.Join(t.TempDir(), "paint.txt")
	if err := os.WriteFile(path, []byte(paintArt), 0o644); err != nil {
		t.Fatal(err)
	}
	return Config{
		TBAddrs:       liveAddrs,
		Cluster:       0,
		GridW:         paintGrid,
		GridH:         paintGrid,
		BlueprintPath: path,
		PaintWorkers:  4,
		PaintOrder:    "scanline",
	}
}

// TestPaintBlueprintDirect paints a 2x2 blueprint through the full
// bot.Run painter path in direct mode and verifies on the ledger that every
// pixel received a posted claim leg with the expected color. A second run
// over the same pixels must succeed too (pixels are repaintable by design —
// confirm posts + refunds the unit).
func TestPaintBlueprintDirect(t *testing.T) {
	cfg := painterConfig(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	m, err := Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("paint run 1: %v", err)
	}
	t.Logf("run 1: total=%d painted=%d confirmed=%d claims=%d conflicts=%d errors=%d",
		m.Total, m.Painted.Load(), m.Confirmed.Load(), m.ClaimsStarted.Load(), m.LockConflicts.Load(), m.Errors.Load())
	if m.Painted.Load() != 4 || m.Confirmed.Load() != 4 || m.Errors.Load() != 0 {
		t.Fatalf("run 1: painted=%d confirmed=%d errors=%d, want 4/4/0",
			m.Painted.Load(), m.Confirmed.Load(), m.Errors.Load())
	}

	// Ledger truth: every painted pixel has a posted claim leg with the
	// artist's color. Pixels land at (7,7)+(0,0),(1,0),(0,1),(1,1) on the
	// 16x16 grid (centered 2x2). k=#222222→3, w=#ffffff→0.
	c, err := tbclient.Connect(0, liveAddrs)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got := map[[2]uint32]uint8{}
	transfers, err := c.QueryCanvasTransfers(0, 2000)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for _, tr := range transfers {
		if x, y, color, ok := tbclient.IsPostedClaim(tr, paintGrid, paintGrid); ok {
			got[[2]uint32{x, y}] = color
		}
	}
	want := [][2]uint32{{7, 7}, {8, 7}, {7, 8}, {8, 8}}
	wantColor := map[[2]uint32]uint8{
		{7, 7}: 3, {8, 7}: 0, // k, w
		{7, 8}: 0, {8, 8}: 3, // w, k
	}
	for _, p := range want {
		if got[p] != wantColor[p] {
			t.Fatalf("pixel %v: color %d on ledger, want %d", p, got[p], wantColor[p])
		}
	}

	// Second identical run: repaintability + idempotent provisioning.
	m2, err := Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("paint run 2: %v", err)
	}
	if m2.Painted.Load() != 4 || m2.Errors.Load() != 0 {
		t.Fatalf("run 2: painted=%d errors=%d, want 4/0", m2.Painted.Load(), m2.Errors.Load())
	}
}

// TestPaintBlueprintOutOfBounds asserts the fail-fast path: a blueprint that
// does not fit the grid errors before any claim is submitted.
func TestPaintBlueprintOutOfBounds(t *testing.T) {
	if !liveReady {
		t.Skip("live TigerBeetle cluster unavailable")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// 3x3 blueprint at 15,15 on a 16x16 grid → overflows.
	cfg := painterConfig(t)
	cfg.PaintOffset = [2]uint32{15, 15}
	cfg.PaintOffsetSet = true
	if _, err := Run(context.Background(), cfg, log); err == nil {
		t.Fatal("want out-of-bounds error, got nil")
	}
}
