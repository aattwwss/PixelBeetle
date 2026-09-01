package bot

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makePNG encodes an image.Image to a PNG byte blob (the decoder path tests
// exercise is real: bytes → image.Decode, not direct use of the image).
func makePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func solidRGBA(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func TestFitScale(t *testing.T) {
	cases := []struct {
		w, h, maxW, maxH, wantW, wantH int
	}{
		{6, 4, 3, 3, 3, 2}, // aspect 1.5, width binds
		{4, 6, 3, 3, 2, 3}, // height binds
		{3, 3, 3, 3, 3, 3}, // exact fit, no scale
		{2, 2, 3, 3, 2, 2}, // smaller image: never upscale
		{100, 50, 10, 100, 10, 5},
		{1000, 500, 100, 100, 100, 50},
		{1, 100, 10, 10, 1, 10},
	}
	for _, c := range cases {
		dw, dh := fitScale(c.w, c.h, c.maxW, c.maxH)
		if dw != c.wantW || dh != c.wantH {
			t.Errorf("fitScale(%d,%d,%d,%d) = %dx%d, want %dx%d",
				c.w, c.h, c.maxW, c.maxH, dw, dh, c.wantW, c.wantH)
		}
	}
	// Degenerate: extremely wide input into a tall box still yields 1px.
	if dw, dh := fitScale(1000, 1, 10, 10); dw != 10 || dh != 1 {
		t.Errorf("thin strip = %dx%d, want 10x1", dw, dh)
	}
}

func TestImageSolidBlock(t *testing.T) {
	data := makePNG(t, solidRGBA(2, 2, color.RGBA{0xff, 0xff, 0xff, 0xff}))
	cfg := Config{GridW: 16, GridH: 16, BlueprintPath: writeTemp(t, "b.png", data)}
	bp, err := LoadPaint(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if bp.W != 2 || bp.H != 2 || len(bp.Placements) != 4 {
		t.Fatalf("solid block = %dx%d with %d placements", bp.W, bp.H, len(bp.Placements))
	}
	for _, p := range bp.Placements {
		if p.Color != 0 { // #ffffff → 0
			t.Errorf("pixel %+v color = %d, want 0", p, p.Color)
		}
	}
}

func TestImageAlphaBlend(t *testing.T) {
	// 50% white over the #1c1c1c canvas ≈ 142 gray → palette 2 (#888888).
	data := makePNG(t, solidRGBA(1, 1, color.RGBA{0xff, 0xff, 0xff, 0x80}))
	cfg := Config{GridW: 16, GridH: 16, BlueprintPath: writeTemp(t, "a.png", data)}
	bp, err := LoadPaint(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(bp.Placements) != 1 || bp.Placements[0].Color != 2 {
		t.Fatalf("blended pixel = %+v, want single placement of color 2", bp.Placements)
	}
}

func TestImageTransparentSkipped(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{0xff, 0x00, 0x00, 0xff}) // opaque red
	img.Set(1, 0, color.RGBA{0xff, 0x00, 0x00, 0x00}) // fully transparent
	data := makePNG(t, img)
	cfg := Config{GridW: 16, GridH: 16, BlueprintPath: writeTemp(t, "t.png", data)}
	bp, err := LoadPaint(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(bp.Placements) != 1 {
		t.Fatalf("placements = %d, want 1 (transparent skipped): %v", len(bp.Placements), bp.Placements)
	}
	if p := bp.Placements[0]; p.X != 0 || p.Y != 0 || p.Color != 7 { // pure red → #ba2d2d red (7)
		t.Fatalf("opaque pixel = %+v, want (0,0) color 7", p)
	}
}

func TestImageBoxFilterAndAspect(t *testing.T) {
	// 6x4 image: left 3 columns white, right 3 columns black → into a 3x3
	// box (fit → 3x2). Dest col 0 = src cols 0-1 (white), col 1 = cols 2-3
	// (white|black → gray), col 2 = cols 4-5 (black). This exercises both
	// box averaging and aspect preservation in one test.
	img := image.NewRGBA(image.Rect(0, 0, 6, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 6; x++ {
			c := color.RGBA{0xff, 0xff, 0xff, 0xff}
			if x >= 3 {
				c = color.RGBA{0x00, 0x00, 0x00, 0xff}
			}
			img.Set(x, y, c)
		}
	}
	data := makePNG(t, img)
	cfg := Config{GridW: 16, GridH: 16, PaintSize: [2]uint32{3, 3}, PaintSizeSet: true,
		BlueprintPath: writeTemp(t, "f.png", data)}
	bp, err := LoadPaint(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if bp.W != 3 || bp.H != 2 {
		t.Fatalf("fitted = %dx%d, want 3x2", bp.W, bp.H)
	}
	got := placementSet(bp.Placements)
	row0 := []uint8{got[[2]int{0, 0}], got[[2]int{1, 0}], got[[2]int{2, 0}]}
	want := []uint8{0, 2, 3} // white, gray(averaged), dark(#2... nearest to black)
	if row0[0] != want[0] || row0[1] != want[1] || row0[2] != want[2] {
		t.Errorf("dest row 0 colors = %v, want %v", row0, want)
	}
}

func TestInspectRoundTrip(t *testing.T) {
	// An image with several colors → blueprint → FormatTextArt → ParseTextArt
	// must reproduce the same placements (the -inspect hand-edit loop).
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	colors := []color.RGBA{
		{0xff, 0xff, 0xff, 0xff}, // white → 0
		{0xba, 0x2d, 0x2d, 0xff}, // red   → 7
		{0x43, 0x63, 0xd8, 0xff}, // blue  → 12
	}
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, colors[(x+y)%3]) // so every color appears
		}
	}
	data := makePNG(t, img)
	cfg := Config{GridW: 16, GridH: 16, BlueprintPath: writeTemp(t, "r.png", data)}
	bp, err := LoadPaint(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	art := FormatTextArt(bp)
	bp2, err := ParseTextArt([]byte(art))
	if err != nil {
		t.Fatalf("parse emitted art: %v\n%s", err, art)
	}
	got1, got2 := placementSet(bp.Placements), placementSet(bp2.Placements)
	if bp.W != bp2.W || bp.H != bp2.H || len(got1) != len(got2) {
		t.Fatalf("round-trip mismatch: %dx%d/%d vs %dx%d/%d\n---\n%s",
			bp.W, bp.H, len(got1), bp2.W, bp2.H, len(got2), art)
	}
	for cell, c := range got1 {
		if got2[cell] != c {
			t.Fatalf("round-trip cell %v color %d vs %d\n%s", cell, c, got2[cell], art)
		}
	}
}

func TestFormatTextArtDeterministic(t *testing.T) {
	bp, _ := ParseTextArt([]byte(testArt))
	art := FormatTextArt(bp)
	// Most-used color gets 'a': testArt has w×4 (color 0), k×2 (3), b×1 (12).
	if !strings.Contains(art, "legend: a=#ffffff b=#222222 c=#4363d8") {
		t.Errorf("legend ordering wrong:\n%s", art)
	}
	// Re-running is stable.
	if art != FormatTextArt(bp) {
		t.Error("FormatTextArt not deterministic")
	}
}

func TestLoadPaintDispatch(t *testing.T) {
	// .txt → ParseTextArt; images → pipeline; unsupported ext → error.
	txtCfg := Config{GridW: 16, GridH: 16, BlueprintPath: writeTemp(t, "a.txt", []byte(testArt))}
	bp, err := LoadPaint(txtCfg)
	if err != nil || bp.W != 3 || bp.H != 3 || len(bp.Placements) != 6 {
		t.Fatalf("txt dispatch = %+v, %v", bp, err)
	}
	imgCfg := Config{GridW: 16, GridH: 16, BlueprintPath: writeTemp(t, "a.jpg", makePNG(t, solidRGBA(1, 1, color.RGBA{1, 2, 3, 255})))}
	if _, err := LoadPaint(imgCfg); err != nil {
		t.Fatalf("jpg dispatch: %v", err)
	}
	badCfg := Config{GridW: 16, GridH: 16, BlueprintPath: writeTemp(t, "a.bin", []byte("nope"))}
	if _, err := LoadPaint(badCfg); err == nil || !strings.Contains(err.Error(), "unsupported blueprint file") {
		t.Fatalf("want unsupported-ext error, got %v", err)
	}
}

func TestLoadPaintFileAndDraws(t *testing.T) {
	// -paint file + -draw overlay: draw paint wins at the shared cell, and
	// the file's other cells survive.
	cfg := Config{
		GridW: 256, GridH: 256,
		BlueprintPath: writeTemp(t, "c.txt", []byte(testArt)),
		Draws:         []string{"fillrect:0,0,2,2,#ffd600"},
	}
	bp, err := LoadPaint(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := placementSet(bp.Placements)
	if got[[2]int{0, 0}] != 8 { // w(0) overridden by yellow fill
		t.Errorf("overlaid cell = %d, want 8", got[[2]int{0, 0}])
	}
	if got[[2]int{1, 2}] != 12 { // b untouched
		t.Errorf("uncovered cell = %d, want 12", got[[2]int{1, 2}])
	}
}

// writeTemp writes data to a temp file with the given name and returns its
// path (files live in the test's temp dir).
func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
