package bot

import "fmt"

// Palette is the 16-color canvas palette, mirroring
// internal/web/static/palette.js — that file is the single source of truth.
// A blueprint Placement.Color and the claim color sent on the wire are
// indices into this table (0-15); the server presents and stores them via
// the same numbering (bitmap byte = index+1). Keep in sync with PALETTE.
var palette = [16][3]uint8{
	{0xff, 0xff, 0xff}, // 0  white
	{0xe4, 0xe4, 0xe4}, // 1  light gray
	{0x88, 0x88, 0x88}, // 2  gray
	{0x22, 0x22, 0x22}, // 3  dark
	{0xff, 0xb4, 0x70}, // 4  peach
	{0x9a, 0x63, 0x24}, // 5  brown
	{0x80, 0x00, 0x00}, // 6  maroon
	{0xba, 0x2d, 0x2d}, // 7  red
	{0xff, 0xd6, 0x00}, // 8  yellow
	{0x80, 0x80, 0x00}, // 9  olive
	{0x46, 0x99, 0x90}, // 10 teal
	{0x42, 0xd4, 0xf4}, // 11 cyan
	{0x43, 0x63, 0xd8}, // 12 blue
	{0x00, 0x00, 0x75}, // 13 navy
	{0xf0, 0x32, 0xe6}, // 14 magenta
	{0xfa, 0xbe, 0xd4}, // 15 pink
}

// parseHex parses "#rrggbb" (exactly six hex digits). No abbreviated or
// non-hex forms — blueprint authoring mistakes should be loud.
func parseHex(s string) (r, g, b uint8, ok bool) {
	if len(s) != 7 || s[0] != '#' {
		return 0, 0, 0, false
	}
	var v [3]uint8
	for i := 0; i < 3; i++ {
		hi, hok := hexNibble(s[1+2*i])
		lo, lok := hexNibble(s[2+2*i])
		if !hok || !lok {
			return 0, 0, 0, false
		}
		v[i] = hi<<4 | lo
	}
	return v[0], v[1], v[2], true
}

func hexNibble(c byte) (uint8, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// nearestColor maps a hex color to the palette index closest to it by the
// perceptually-weighted distance 2·Δr² + 4·Δg² + 3·Δb² (Rec.601-ish weights:
// the eye is ~2× more sensitive to green than blue). This is also the
// quantizer Phase 3 (image import) will reuse.
func nearestColor(r, g, b uint8) uint8 {
	best, bestD := uint8(0), uint32(1<<32-1)
	for i, p := range palette {
		dr, dg, db := int32(r)-int32(p[0]), int32(g)-int32(p[1]), int32(b)-int32(p[2])
		d := uint32(2*dr*dr + 4*dg*dg + 3*db*db)
		if d < bestD {
			best, bestD = uint8(i), d
		}
	}
	return best
}

// PaletteColor resolves a blueprint legend hex to a palette index, erroring
// loudly for anything that is not a well-formed six-digit hex color.
func PaletteColor(hex string) (uint8, error) {
	r, g, b, ok := parseHex(hex)
	if !ok {
		return 0, fmt.Errorf("invalid color %q: want #rrggbb (six hex digits)", hex)
	}
	return nearestColor(r, g, b), nil
}
