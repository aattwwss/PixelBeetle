package bot

import (
	"strings"
	"testing"
)

// placementSet builds a (x,y)→color map for order-insensitive assertions.
func placementSet(ps []Placement) map[[2]int]uint8 {
	m := make(map[[2]int]uint8, len(ps))
	for _, p := range ps {
		m[[2]int{p.X, p.Y}] = p.Color
	}
	return m
}

func TestRectOutline(t *testing.T) {
	ps, err := parseShape("rect:2,2,4,3,#ffffff") // 4 wide × 3 tall outline
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ps) != 10 { // perimeter: 2*(4+3)-4
		t.Fatalf("rect outline = %d placements, want 10: %v", len(ps), ps)
	}
	got := placementSet(ps)
	for _, p := range [][2]int{{2, 2}, {5, 2}, {2, 4}, {5, 4}, {3, 4}} {
		if _, ok := got[p]; !ok {
			t.Errorf("missing outline pixel %v", p)
		}
	}
	if _, ok := got[[2]int{3, 3}]; ok { // interior must be empty
		t.Errorf("interior pixel (3,3) painted by outline rect")
	}
	if got[[2]int{2, 2}] != 0 { // #ffffff → palette 0
		t.Errorf("color = %d, want 0", got[[2]int{2, 2}])
	}
}

func TestFillRect(t *testing.T) {
	ps, err := parseShape("fillrect:0,0,2,3,#ba2d2d")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ps) != 6 {
		t.Fatalf("fillrect = %d placements, want 6", len(ps))
	}
	got := placementSet(ps)
	for x := 0; x < 2; x++ {
		for y := 0; y < 3; y++ {
			if got[[2]int{x, y}] != 7 { // #ba2d2d → 7
				t.Errorf("pixel (%d,%d) = %d, want 7", x, y, got[[2]int{x, y}])
			}
		}
	}
}

func TestCircleMidpointRing(t *testing.T) {
	ps, err := parseShape("circle:4,4,3,#ffd600")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := placementSet(ps)
	// Golden set for radius 3 at (4,4): the scanline-deduped midpoint walk
	// (16 raster pixels — including the octant (2,2)-family the mathematical
	// ring omits; that is the standard integer-circle rasterization).
	want := map[[2]int]uint8{
		{7, 4}: 8, {4, 7}: 8, {1, 4}: 8, {4, 1}: 8,
		{7, 5}: 8, {5, 7}: 8, {3, 7}: 8, {1, 5}: 8,
		{1, 3}: 8, {3, 1}: 8, {5, 1}: 8, {7, 3}: 8,
		{6, 6}: 8, {2, 6}: 8, {2, 2}: 8, {6, 2}: 8,
	}
	if len(got) != len(want) {
		t.Fatalf("circle r=3 = %d points, want %d: %v", len(ps), len(want), ps)
	}
	for p, c := range want {
		if got[p] != c {
			t.Errorf("ring pixel %v = %d, want %d", p, got[p], c)
		}
	}
	// No duplicates (dedup works) and placements are scanline-ordered.
	for i := 1; i < len(ps); i++ {
		if ps[i].Y < ps[i-1].Y || (ps[i].Y == ps[i-1].Y && ps[i].X < ps[i-1].X) {
			t.Errorf("placements not scanline-ordered at %d: %v then %v", i, ps[i-1], ps[i])
		}
	}
}

func TestLineBresenham(t *testing.T) {
	ps, err := parseShape("line:0,0,3,0,#222222")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ps) != 4 {
		t.Fatalf("horizontal line = %d points, want 4", len(ps))
	}
	for x, p := range ps {
		if p.X != x || p.Y != 0 || p.Color != 3 {
			t.Errorf("point %d = %+v, want (x=%d,y=0,color=3)", x, p, x)
		}
	}
	// Inclusive diagonal.
	ps, err = parseShape("line:0,0,2,2,#222222")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ps) != 3 {
		t.Fatalf("diagonal = %d points, want 3", len(ps))
	}
}

func TestTextGlyphLayout(t *testing.T) {
	ps, err := parseShape("text:0,0,L,#222222")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := placementSet(ps)
	// L = left column full height + bottom row.
	if got[[2]int{0, 0}] != 3 || got[[2]int{0, 6}] != 3 || got[[2]int{4, 6}] != 3 {
		t.Errorf("L glyph missing expected pixels: %v", ps)
	}
	if _, ok := got[[2]int{1, 0}]; ok {
		t.Errorf("L glyph row 0 has a pixel at x=1 (should be only the column)")
	}
	// Lowercase input uppercased.
	ps2, err := parseShape("text:10,0,l,#222222")
	if err != nil {
		t.Fatalf("parse lowercase: %v", err)
	}
	if len(ps) != len(ps2) {
		t.Errorf("uppercase L and lowercase l differ in size: %d vs %d", len(ps), len(ps2))
	}
}

func TestTextAdvanceAndLength(t *testing.T) {
	ps, _ := parseShape("text:0,0,AB,#222222")
	got := placementSet(ps)
	// 'A' starts at x=0, 'B' at x=6 (glyphAdvance); last B pixel at x=6+4=10.
	if got[[2]int{0, 3}] != 3 || got[[2]int{6, 0}] != 3 || got[[2]int{10, 1}] != 3 {
		t.Errorf("AB text advance wrong: %v", ps)
	}
	if _, err := parseShape("text:0,0,ABCDEFGHIJKLMNOPQRSTUVWXYZABCDEFG,#222222"); err == nil {
		t.Error("want error for 33-glyph text, got nil")
	}
}

func TestParseShapeErrors(t *testing.T) {
	cases := []struct {
		spec    string
		wantSub string
	}{
		{"thing:1,2,#ffffff", "unknown shape"},
		{"rect:0,0,2,#ffffff", "rect wants"},
		{"circle:5,5,0,#ffffff", "zero radius"},
		{"fillrect:0,0,0,5,#ffffff", "zero-size"},
		{"line:0,0,1,#ffffff", "line wants"},
		{"text:0,0,#ffffff", "text wants"},
		{"text:0,0,HELLO", "text wants"},
		{"rect:-1,0,2,2,#ffffff", "non-negative"},
		{"rect:0,0,2,2,#gggggg", "invalid color"},
		{"rect:0,0,2,2,banana", "invalid color"},
		{"thing:1,2", "invalid color"}, // color is validated before the shape name
		{"", "want name"},
	}
	for _, c := range cases {
		_, err := parseShape(c.spec)
		if err == nil {
			t.Errorf("%q: want error containing %q, got nil", c.spec, c.wantSub)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSub) {
			t.Errorf("%q: error %q does not contain %q", c.spec, err, c.wantSub)
		}
	}
}

func TestFontTableInvariant(t *testing.T) {
	// Every glyph is exactly 7 rows × 5 cols of {'.','X'} — the renderer and
	// the drawing spacing both assume this shape.
	for r, rows := range fontGlyphs {
		if len(rows) != glyphH {
			t.Errorf("glyph %q has %d rows, want %d", r, len(rows), glyphH)
		}
		for y, row := range rows {
			if len(row) != glyphW {
				t.Errorf("glyph %q row %d is %d chars, want %d (%q)", r, y, len(row), glyphW, row)
			}
			for x := range row {
				if row[x] != '.' && row[x] != 'X' {
					t.Errorf("glyph %q row %d char %d = %q, want '.' or 'X'", r, y, x, row[x])
				}
			}
		}
	}
	if _, ok := fontGlyphs[' ']; !ok {
		t.Error("font missing space glyph")
	}
	// Sanity: the covered glyph set is what the task promised.
	for _, r := range "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.,:!?'-()/#+=*" {
		if _, ok := fontGlyphs[r]; !ok {
			t.Errorf("font missing glyph %q", r)
		}
	}
}

func TestComposeShapesOverlay(t *testing.T) {
	// A fillrect then a smaller fillrect overlapping it: later wins.
	ps, err := ComposeShapes([]string{
		"fillrect:0,0,5,5,#4363d8",
		"fillrect:1,1,3,3,#ffd600",
	})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	got := placementSet(ps)
	if got[[2]int{1, 1}] != 8 { // overlapped → yellow wins
		t.Errorf("overlap pixel = %d, want 8 (later draw wins)", got[[2]int{1, 1}])
	}
	if got[[2]int{0, 0}] != 12 { // outside overlap → blue stays
		t.Errorf("non-overlap pixel = %d, want 12", got[[2]int{0, 0}])
	}
	if len(got) != 25 { // 5x5 with no duplicate cells
		t.Errorf("compose = %d cells, want 25", len(got))
	}
}

func TestComposeShapesError(t *testing.T) {
	// ComposeShapes surfaces the first bad spec; Run prefixes it with "draw:".
	_, err := ComposeShapes([]string{"rect:0,0,1,1,#ffffff", "bogus:1,#ffffff", "rect:2,2,1,1,#ffffff"})
	if err == nil || !strings.Contains(err.Error(), "unknown shape") {
		t.Fatalf("want unknown-shape error, got %v", err)
	}
}

func TestDrawOnlyBlueprintBounds(t *testing.T) {
	// A -draw-only blueprint's W/H come from the shapes' bounding box, so
	// paint mode auto-centering works without a -paint file.
	bp, err := LoadPaint(Config{GridW: 256, GridH: 256, Draws: []string{"rect:0,0,10,5,#ffffff", "line:0,0,9,0,#222222"}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if bp.W != 10 || bp.H != 5 {
		t.Fatalf("bounds = %dx%d, want 10x5", bp.W, bp.H)
	}
	if err := bp.ValidateBounds(256, 256, [2]uint32{120, 125}); err != nil {
		t.Fatalf("centered-ish offset should fit: %v", err)
	}
}
