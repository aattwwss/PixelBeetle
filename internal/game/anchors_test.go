package game

import "testing"

// TestAnchorGridRetention pins the entry cap (anchorMax): every insert past
// the cap evicts exactly the overflow, so the list sits steady at anchorMax.
// Regression for a bug where the eviction loop never changed its own
// condition and mass-evicted down to a single entry whenever the cap was
// crossed (RAM window collapse + a ~1440-record sidecar burst per boundary).
func TestAnchorGridRetention(t *testing.T) {
	var g anchorGrid
	bmp := make([]byte, 16) // tiny "canvas"
	evicted := 0
	g.onEvict = func(tsMs int64, hash uint64, b []byte) (int64, uint32, error) {
		evicted++
		return 0, 0, nil
	}

	total := anchorMax * 2
	for i := 0; i < total; i++ {
		// Vary the bitmap so most minutes allocate a distinct pooled blob
		// (hash-dedup would otherwise collapse every boundary to one entry).
		bmp[i%len(bmp)]++
		g.insert(int64(i)*anchorIntervalMs, bmp)
	}

	if len(g.list) != anchorMax {
		t.Fatalf("steady-state list len %d, want %d (must sit exactly at the cap, not collapse)", len(g.list), anchorMax)
	}
	if len(g.list) == 1 {
		t.Fatal("retention collapsed to a single anchor")
	}
	if want := total - anchorMax; evicted != want {
		t.Fatalf("total evictions %d, want %d (incremental overflow, not a mass sweep)", evicted, want)
	}
}

// TestAnchorGridByteCapNeverEvictsTheLastAnchor: when the byte cap binds and
// every dropped anchor shares its bitmap with survivors (a quiet canvas), the
// eviction loop may trim the list far down — but it must always keep at
// least the newest anchor as the seek starting point.
func TestAnchorGridByteCapNeverEvictsTheLastAnchor(t *testing.T) {
	var g anchorGrid
	bmp := make([]byte, 16)
	g.poolBytes = maxAnchorPoolSize + 1 // force the byte branch of evict
	for i := 0; i < 10; i++ {
		g.insert(int64(i)*anchorIntervalMs, bmp) // identical state: one shared blob
	}
	if len(g.list) < 1 {
		t.Fatal("byte-cap eviction dropped the final anchor")
	}
	if g.list[len(g.list)-1].TsMs != 9*anchorIntervalMs {
		t.Fatalf("newest anchor was evicted: %+v", g.list)
	}
}
