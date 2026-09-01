package bot

import "testing"

func TestPaletteIsSixteenUniqueColors(t *testing.T) {
	if len(palette) != 16 {
		t.Fatalf("len(palette) = %d, want 16", len(palette))
	}
	seen := map[[3]uint8]bool{}
	for _, c := range palette {
		if seen[c] {
			t.Fatalf("duplicate palette color %v", c)
		}
		seen[c] = true
	}
}

func TestPaletteColorExactMatch(t *testing.T) {
	// Every palette hex maps back to its own index.
	hexes := []string{"#ffffff", "#e4e4e4", "#888888", "#222222", "#ffb470", "#9a6324", "#800000", "#ba2d2d",
		"#ffd600", "#808000", "#469990", "#42d4f4", "#4363d8", "#000075", "#f032e6", "#fabed4"}
	for i, h := range hexes {
		idx, err := PaletteColor(h)
		if err != nil {
			t.Fatalf("PaletteColor(%s): %v", h, err)
		}
		if idx != uint8(i) {
			t.Fatalf("PaletteColor(%s) = %d, want %d", h, idx, i)
		}
	}
}

func TestPaletteColorNearestMatch(t *testing.T) {
	// #010101 is darkest → index 3 (#222222). #fefefe is near white → 0.
	idx, err := PaletteColor("#010101")
	if err != nil || idx != 3 {
		t.Fatalf("PaletteColor(#010101) = %d, %v; want 3", idx, err)
	}
	idx, err = PaletteColor("#fefefe")
	if err != nil || idx != 0 {
		t.Fatalf("PaletteColor(#fefefe) = %d, %v; want 0", idx, err)
	}
	// #1c1c1c (canvas empty color) → nearest dark, index 3.
	idx, err = PaletteColor("#1c1c1c")
	if err != nil || idx != 3 {
		t.Fatalf("PaletteColor(#1c1c1c) = %d, %v; want 3", idx, err)
	}
}

func TestPaletteColorRejectsBadHex(t *testing.T) {
	for _, bad := range []string{"red", "#12345", "#1234567", "#gggggg", "123456", "#12345g"} {
		if _, err := PaletteColor(bad); err == nil {
			t.Fatalf("PaletteColor(%q): want error, got nil", bad)
		}
	}
}
