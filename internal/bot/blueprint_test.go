package bot

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

const testArt = `legend: k=#222222 w=#ffffff b=#4363d8
ww
kwk
.b.`

func TestParseTextArtBasic(t *testing.T) {
	bp, err := ParseTextArt([]byte(testArt))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if bp.W != 3 || bp.H != 3 {
		t.Fatalf("bounds = %dx%d, want 3x3", bp.W, bp.H)
	}
	// Scanline order: (0,0)w (1,0)w (0,1)k (1,1)w (2,1)k (1,2)b — dots skipped.
	// Note: 'w' is #ffffff = palette index 0; 'k' #222222 → 3; 'b' #4363d8 → 12.
	want := []Placement{
		{X: 0, Y: 0, Color: 0},  // w
		{X: 1, Y: 0, Color: 0},  // w
		{X: 0, Y: 1, Color: 3},  // k
		{X: 1, Y: 1, Color: 0},  // w
		{X: 2, Y: 1, Color: 3},  // k
		{X: 1, Y: 2, Color: 12}, // b
	}
	if len(bp.Placements) != len(want) {
		t.Fatalf("placements = %d, want %d: %v", len(bp.Placements), len(want), bp.Placements)
	}
	for i, w := range want {
		got := bp.Placements[i]
		if got.X != w.X || got.Y != w.Y || got.Color != w.Color {
			t.Fatalf("placement[%d] = %+v, want %+v", i, got, w)
		}
	}
}

func TestParseTextArtLeadingSpacesWidenBox(t *testing.T) {
	// Leading-space padding is part of the art: the bounding box includes
	// those columns (so -paint-offset aligns the whole drawing). Dot/space
	// skip placements, not columns.
	bp, err := ParseTextArt([]byte("legend: k=#222222\n  k"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if bp.W != 3 {
		t.Fatalf("W = %d, want 3 (two padding columns + k)", bp.W)
	}
	if len(bp.Placements) != 1 || bp.Placements[0].X != 2 {
		t.Fatalf("placements = %v, want single k at x=2", bp.Placements)
	}
}

func TestParseTextArtTrailingNewlineAndCRLF(t *testing.T) {
	bp, err := ParseTextArt([]byte(testArt + "\n"))
	if err != nil {
		t.Fatalf("trailing newline: %v", err)
	}
	if len(bp.Placements) != 6 {
		t.Fatalf("trailing newline placements = %d, want 6", len(bp.Placements))
	}
	bp, err = ParseTextArt([]byte("legend: k=#222222\r\nkk\r\n"))
	if err != nil {
		t.Fatalf("CRLF: %v", err)
	}
	if len(bp.Placements) != 2 || bp.Placements[0].X != 0 || bp.Placements[1].X != 1 {
		t.Fatalf("CRLF placements = %v, want (0,0),(1,0)", bp.Placements)
	}
}

func TestParseTextArtRaggedLines(t *testing.T) {
	// Short row just leaves the rest of its canvas row un-touched.
	bp, err := ParseTextArt([]byte("legend: k=#222222\nkk\nkkkkkk"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if bp.W != 6 || bp.H != 2 {
		t.Fatalf("bounds = %dx%d, want 6x2", bp.W, bp.H)
	}
	if len(bp.Placements) != 8 {
		t.Fatalf("placements = %d, want 8", len(bp.Placements))
	}
}

func TestParseTextArtNoLegendAllSkip(t *testing.T) {
	bp, err := ParseTextArt([]byte("..\n.."))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(bp.Placements) != 0 {
		t.Fatalf("placements = %d, want 0 (all dots skipped)", len(bp.Placements))
	}
	if bp.W != 2 || bp.H != 2 {
		t.Fatalf("bounds = %dx%d, want 2x2", bp.W, bp.H)
	}
}

func TestParseTextArtUnknownCharReportsPosition(t *testing.T) {
	_, err := ParseTextArt([]byte("legend: k=#222222\nk?k"))
	if err == nil {
		t.Fatal("want error for unknown char, got nil")
	}
	want := "line 2 col 2"
	for i := 0; i+len(want) <= len(err.Error()); i++ {
		if err.Error()[i:i+len(want)] == want {
			return
		}
	}
	t.Fatalf("error %q does not mention %q", err, want)
}

func TestParseTextArtBadLegend(t *testing.T) {
	if _, err := ParseTextArt([]byte("legend: k=#12345\nk")); err == nil {
		t.Fatal("want error for short hex, got nil")
	}
	if _, err := ParseTextArt([]byte("legend: .=#ffffff\nk")); err == nil {
		t.Fatal("want error for '.' reserved as legend char, got nil")
	}
	if _, err := ParseTextArt([]byte("legend: k=#ffffff\n#fff")); err == nil {
		t.Fatal("want error for char without legend entry, got nil")
	}
}

func TestValidateBounds(t *testing.T) {
	bp := Blueprint{W: 10, H: 4}
	if err := bp.ValidateBounds(16, 16, [2]uint32{0, 0}); err != nil {
		t.Fatalf("expected fit, got %v", err)
	}
	if err := bp.ValidateBounds(16, 16, [2]uint32{7, 0}); err == nil {
		t.Fatal("want error when width overflows, got nil")
	}
	if err := bp.ValidateBounds(16, 16, [2]uint32{0, 13}); err == nil {
		t.Fatal("want error when height overflows, got nil")
	}
	if err := bp.ValidateBounds(16, 16, [2]uint32{6, 12}); err != nil {
		t.Fatalf("expected exact fit at corner, got %v", err)
	}
}

// shuffledPlacements mirrors paintRun's one-time random reorder; the test
// asserts the multiset of placements is preserved (same pixels, same colors).
func shuffledPlacements(t *testing.T, ps []Placement) []Placement {
	t.Helper()
	out := append([]Placement(nil), ps...)
	for i := 0; i < 3; i++ {
		shufflePlacements(out)
		if !sameMultiset(t, ps, out) {
			t.Fatalf("shuffle changed the placement multiset")
		}
	}
	return out
}

func sameMultiset(t *testing.T, a, b []Placement) bool {
	t.Helper()
	if len(a) != len(b) {
		return false
	}
	as := append([]Placement(nil), a...)
	bs := append([]Placement(nil), b...)
	sort.Slice(as, func(i, j int) bool {
		if as[i].X != as[j].X {
			return as[i].X < as[j].X
		}
		return as[i].Y < as[j].Y
	})
	sort.Slice(bs, func(i, j int) bool {
		if bs[i].X != bs[j].X {
			return bs[i].X < bs[j].X
		}
		return bs[i].Y < bs[j].Y
	})
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func TestShufflePreservesPlacementMultiset(t *testing.T) {
	ps := []Placement{
		{0, 0, 1}, {1, 0, 3}, {2, 0, 1}, {0, 1, 3}, {1, 1, 12},
	}
	shuffledPlacements(t, ps)
}

func TestExampleBlueprintsParse(t *testing.T) {
	// The shipped examples must stay valid: any legend typo or unknown char
	// would otherwise only surface at paint time.
	entries, err := os.ReadDir("../../examples")
	if err != nil {
		t.Skip("skipping: examples dir not visible from here")
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".txt" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("../../examples", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		bp, err := ParseTextArt(data)
		if err != nil {
			t.Fatalf("example %s: %v", e.Name(), err)
		}
		if len(bp.Placements) == 0 {
			t.Fatalf("example %s: zero placements", e.Name())
		}
	}
}
