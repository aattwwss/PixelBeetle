package bot

import (
	"strings"
	"testing"

	"pixelbeetle/internal/canvas"
)

func TestRenderPreviewEmptyCanvas(t *testing.T) {
	bmp := canvas.NewBitmap(8, 4)
	bp, err := ParseTextArt([]byte("legend: k=#222222\nkk\nkk\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := RenderPreview(bmp, 8, 4, bp, [2]uint32{1, 1})
	want := "........\n.dd.....\n.dd.....\n........\n"
	if got != want {
		t.Fatalf("preview mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestRenderPreviewExistingPaintLowercase(t *testing.T) {
	bmp := canvas.NewBitmap(6, 3)
	// palette index 2 (#888888) at (0,0): bmp stores index+1 = 3.
	bmp.Set(0, 0, 3)
	bp, err := ParseTextArt([]byte("legend: k=#222222\nk\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := RenderPreview(bmp, 6, 3, bp, [2]uint32{2, 1})
	want := "c.....\n..d...\n......\n"
	if got != want {
		t.Fatalf("preview mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestRenderPreviewCollisionX(t *testing.T) {
	bmp := canvas.NewBitmap(4, 2)
	bmp.Set(1, 0, 3) // existing paint at (1,0)
	bp, err := ParseTextArt([]byte("legend: k=#222222\nkk\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := RenderPreview(bmp, 4, 2, bp, [2]uint32{1, 0})
	want := ".Xd.\n....\n"
	if got != want {
		t.Fatalf("preview mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestRenderPreviewColorLetterMapping(t *testing.T) {
	bmp := canvas.NewBitmap(2, 1)
	// Palette index 0 → 'a', index 15 → 'p'. bmp stores index+1.
	bmp.Set(0, 0, 1)
	bmp.Set(1, 0, 16)
	got := RenderPreview(bmp, 2, 1, Blueprint{}, [2]uint32{0, 0})
	if got != "ap\n" {
		t.Fatalf("letter mapping: got %q want %q", got, "ap\n")
	}
}

func TestResolvePaintOffset(t *testing.T) {
	bp, err := ParseTextArt([]byte("legend: k=#222222\nkkk\n"))
	if err != nil {
		t.Fatal(err)
	}
	// Explicit offset wins.
	off, err := resolvePaintOffset(Config{GridW: 10, GridH: 10, PaintOffset: [2]uint32{2, 3}, PaintOffsetSet: true}, bp)
	if err != nil || off != [2]uint32{2, 3} {
		t.Fatalf("explicit offset: got %v err %v", off, err)
	}
	// Unset → centered ((10-3)/2=3, (10-2)/2=4).
	off, err = resolvePaintOffset(Config{GridW: 10, GridH: 10}, bp)
	if err != nil || off != [2]uint32{3, 4} {
		t.Fatalf("centered: got %v err %v", off, err)
	}
	// Too big for the grid.
	_, err = resolvePaintOffset(Config{GridW: 2, GridH: 2, PaintOffset: [2]uint32{0, 0}, PaintOffsetSet: true}, bp)
	if err == nil || !strings.Contains(err.Error(), "exceeds grid") {
		t.Fatalf("bounds: got err %v", err)
	}
}
