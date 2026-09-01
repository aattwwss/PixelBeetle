package bot

import (
	"fmt"
	"strconv"
	"strings"
)

// Shape primitives (draw-plan.md §3b). A -draw spec is
// "name,args...,#hex" — coordinates are blueprint-relative (the same offset
// space as a -paint file):
//
//	rect x,y,w,h,#hex      outline rectangle
//	fillrect x,y,w,h,#hex  filled rectangle
//	circle cx,cy,r,#hex    outline circle (midpoint algorithm)
//	line x0,y0,x1,y1,#hex  Bresenham line
//	text x,y,String,#hex   5x7 bitmap text (uppercase; no commas in text;
//	                      unknown glyphs are skipped; max 32 glyphs)
//
// Multiple -draw flags compose onto one blueprint; later flags overlay
// earlier art (a placement at an already-painted cell wins).

const (
	glyphW       = 5
	glyphH       = 7
	glyphAdvance = 6 // glyphW + 1 spacing column
	maxTextGlyph = 32
)

// ComposeShapes parses each -draw spec and returns the union of placements.
// Overlays between specs are resolved by compose() at the blueprint level.
func ComposeShapes(draws []string) ([]Placement, error) {
	out := make([]Placement, 0, 64)
	for _, d := range draws {
		ps, err := parseShape(d)
		if err != nil {
			return nil, err // caller prefixes the -draw value
		}
		out = append(out, ps...)
	}
	return out, nil
}

func parseShape(spec string) ([]Placement, error) {
	name, rest, ok := strings.Cut(spec, ":")
	if !ok {
		return nil, fmt.Errorf("want name:arg,...,#hex — got %q", spec)
	}
	args := strings.Split(rest, ",")
	if len(args) < 2 {
		return nil, fmt.Errorf("want name:arg,...,#hex — got %q", spec)
	}

	// text: the string is a bare field, so it can't share the int parsing.
	if name == "text" {
		if len(args) != 4 {
			return nil, fmt.Errorf("text wants x,y,String,#hex — got %q", spec)
		}
		x, err := nonNegInt(args[0], "text x")
		if err != nil {
			return nil, err
		}
		y, err := nonNegInt(args[1], "text y")
		if err != nil {
			return nil, err
		}
		c, err := PaletteColor(args[3])
		if err != nil {
			return nil, err
		}
		s := strings.ToUpper(args[2])
		if s == "" {
			return nil, fmt.Errorf("text: empty string")
		}
		if n := len([]rune(s)); n > maxTextGlyph {
			return nil, fmt.Errorf("text too long (%d glyphs, max %d)", n, maxTextGlyph)
		}
		return renderText(x, y, s, c), nil
	}

	c, err := PaletteColor(args[len(args)-1])
	if err != nil {
		return nil, err
	}
	ints, err := parseInts(args[:len(args)-1])
	if err != nil {
		return nil, err
	}
	switch name {
	case "rect", "fillrect":
		if len(ints) != 4 {
			return nil, fmt.Errorf("%s wants x,y,w,h,#hex — got %q", name, spec)
		}
		x, y, w, h := ints[0], ints[1], ints[2], ints[3]
		if w == 0 || h == 0 {
			return nil, fmt.Errorf("%s: zero-size shape", name)
		}
		var ps []Placement
		if name == "fillrect" {
			for yy := y; yy < y+h; yy++ {
				for xx := x; xx < x+w; xx++ {
					ps = append(ps, Placement{X: xx, Y: yy, Color: c})
				}
			}
			return ps, nil
		}
		// Outline: top/bottom rows + left/right columns (no double corners).
		for xx := x; xx < x+w; xx++ {
			ps = append(ps, Placement{X: xx, Y: y, Color: c}, Placement{X: xx, Y: y + h - 1, Color: c})
		}
		for yy := y + 1; yy < y+h-1; yy++ {
			ps = append(ps, Placement{X: x, Y: yy, Color: c}, Placement{X: x + w - 1, Y: yy, Color: c})
		}
		return ps, nil
	case "circle":
		if len(ints) != 3 {
			return nil, fmt.Errorf("circle wants cx,cy,r,#hex — got %q", spec)
		}
		if ints[2] == 0 {
			return nil, fmt.Errorf("circle: zero radius")
		}
		return circlePoints(ints[0], ints[1], ints[2], c), nil
	case "line":
		if len(ints) != 4 {
			return nil, fmt.Errorf("line wants x0,y0,x1,y1,#hex — got %q", spec)
		}
		return linePoints(ints[0], ints[1], ints[2], ints[3], c), nil
	default:
		return nil, fmt.Errorf("unknown shape %q (want rect|fillrect|circle|line|text)", name)
	}
}

func parseInts(args []string) ([]int, error) {
	out := make([]int, len(args))
	for i, a := range args {
		v, err := strconv.Atoi(a)
		if err != nil || v < 0 {
			return nil, fmt.Errorf("field %d %q: want non-negative integer", i+1, a)
		}
		out[i] = v
	}
	return out, nil
}

func nonNegInt(s, what string) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%s %q: want non-negative integer", what, s)
	}
	return v, nil
}

// circlePoints traces the raster outline of a circle with the midpoint
// algorithm. Points are deduped into a scanline-ordered set — the octant
// walk emits each point multiple times for small radii.
func circlePoints(cx, cy, r int, c uint8) []Placement {
	set := map[[2]int]struct{}{}
	x, y, err := r, 0, 1-r
	for x >= y {
		for _, p := range [][2]int{
			{cx + x, cy + y}, {cx + y, cy + x}, {cx - y, cy + x}, {cx - x, cy + y},
			{cx - x, cy - y}, {cx - y, cy - x}, {cx + y, cy - x}, {cx + x, cy - y},
		} {
			set[p] = struct{}{}
		}
		y++
		if err < 0 {
			err += 2*y + 1
		} else {
			x--
			err += 2*(y-x) + 1
		}
	}
	return setToScanline(set, c)
}

// setToScanline flattens a (x,y) set into scanline-ordered placements.
func setToScanline(set map[[2]int]struct{}, c uint8) []Placement {
	if len(set) == 0 {
		return nil
	}
	minX, maxX, maxY := 1<<30, -1<<30, -1<<30
	for p := range set {
		if p[0] < minX {
			minX = p[0]
		}
		if p[0] > maxX {
			maxX = p[0]
		}
		if p[1] > maxY {
			maxY = p[1]
		}
	}
	ps := make([]Placement, 0, len(set))
	for y := 0; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			if _, ok := set[[2]int{x, y}]; ok {
				ps = append(ps, Placement{X: x, Y: y, Color: c})
			}
		}
	}
	return ps
}

// linePoints traces a line with Bresenham's integer algorithm, inclusive of
// both endpoints.
func linePoints(x0, y0, x1, y1 int, c uint8) []Placement {
	var ps []Placement
	dx := absInt(x1 - x0)
	dy := -absInt(y1 - y0)
	sx, sy := 1, 1
	if x0 > x1 {
		sx = -1
	}
	if y0 > y1 {
		sy = -1
	}
	e := dx + dy
	for {
		ps = append(ps, Placement{X: x0, Y: y0, Color: c})
		if x0 == x1 && y0 == y1 {
			return ps
		}
		e2 := 2 * e
		if e2 >= dy {
			e += dy
			x0 += sx
		}
		if e2 <= dx {
			e += dx
			y0 += sy
		}
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// renderText places the 5x7 glyphs of s at (x,y), advancing glyphAdvance
// columns per glyph. Unknown glyphs are skipped (e.g. lowercase from the
// uppercase conversion collides with nothing) — a gap, not an error.
func renderText(x, y int, s string, c uint8) []Placement {
	var ps []Placement
	for i, r := range s {
		rows, ok := fontGlyphs[r]
		if !ok {
			continue
		}
		for gy, row := range rows {
			for gx, ch := range row {
				if ch == 'X' {
					ps = append(ps, Placement{X: x + i*glyphAdvance + gx, Y: y + gy, Color: c})
				}
			}
		}
	}
	return ps
}

// fontGlyphs is a 5x7 bitmap font: A–Z, 0–9, space, and the punctuation
// .,:!?'-()/#+= plus '*' (a filled block for simple bars). Every glyph is
// exactly 7 rows of 5 chars from {'.','X'} (tested). Classic readable face.
var fontGlyphs = map[rune][7]string{
	' ':  {`.....`, `.....`, `.....`, `.....`, `.....`, `.....`, `.....`},
	'A':  {`.XXX.`, `X...X`, `X...X`, `XXXXX`, `X...X`, `X...X`, `X...X`},
	'B':  {`XXXX.`, `X...X`, `X...X`, `XXXX.`, `X...X`, `X...X`, `XXXX.`},
	'C':  {`.XXXX`, `X....`, `X....`, `X....`, `X....`, `X....`, `.XXXX`},
	'D':  {`XXXX.`, `X...X`, `X...X`, `X...X`, `X...X`, `X...X`, `XXXX.`},
	'E':  {`XXXXX`, `X....`, `X....`, `XXXX.`, `X....`, `X....`, `XXXXX`},
	'F':  {`XXXXX`, `X....`, `X....`, `XXXX.`, `X....`, `X....`, `X....`},
	'G':  {`.XXXX`, `X....`, `X....`, `X.XXX`, `X...X`, `X...X`, `.XXX.`},
	'H':  {`X...X`, `X...X`, `X...X`, `XXXXX`, `X...X`, `X...X`, `X...X`},
	'I':  {`XXXXX`, `..X..`, `..X..`, `..X..`, `..X..`, `..X..`, `XXXXX`},
	'J':  {`..XXX`, `...X.`, `...X.`, `...X.`, `...X.`, `X..X.`, `.XX..`},
	'K':  {`X...X`, `X..X.`, `X.X..`, `XX...`, `X.X..`, `X..X.`, `X...X`},
	'L':  {`X....`, `X....`, `X....`, `X....`, `X....`, `X....`, `XXXXX`},
	'M':  {`X...X`, `XX.XX`, `X.X.X`, `X.X.X`, `X...X`, `X...X`, `X...X`},
	'N':  {`X...X`, `XX..X`, `X.X.X`, `X..XX`, `X...X`, `X...X`, `X...X`},
	'O':  {`.XXX.`, `X...X`, `X...X`, `X...X`, `X...X`, `X...X`, `.XXX.`},
	'P':  {`XXXX.`, `X...X`, `X...X`, `XXXX.`, `X....`, `X....`, `X....`},
	'Q':  {`.XXX.`, `X...X`, `X...X`, `X...X`, `X.X.X`, `X..X.`, `.XX.X`},
	'R':  {`XXXX.`, `X...X`, `X...X`, `XXXX.`, `X.X..`, `X..X.`, `X...X`},
	'S':  {`.XXXX`, `X....`, `X....`, `.XXX.`, `....X`, `....X`, `XXXX.`},
	'T':  {`XXXXX`, `..X..`, `..X..`, `..X..`, `..X..`, `..X..`, `..X..`},
	'U':  {`X...X`, `X...X`, `X...X`, `X...X`, `X...X`, `X...X`, `.XXX.`},
	'V':  {`X...X`, `X...X`, `X...X`, `X...X`, `X...X`, `.X.X.`, `..X..`},
	'W':  {`X...X`, `X...X`, `X...X`, `X.X.X`, `X.X.X`, `XX.XX`, `X...X`},
	'X':  {`X...X`, `X...X`, `.X.X.`, `..X..`, `.X.X.`, `X...X`, `X...X`},
	'Y':  {`X...X`, `X...X`, `.X.X.`, `..X..`, `..X..`, `..X..`, `..X..`},
	'Z':  {`XXXXX`, `....X`, `...X.`, `..X..`, `.X...`, `X....`, `XXXXX`},
	'0':  {`.XXX.`, `X...X`, `X..XX`, `X.X.X`, `XX..X`, `X...X`, `.XXX.`},
	'1':  {`..X..`, `.XX..`, `..X..`, `..X..`, `..X..`, `..X..`, `XXXXX`},
	'2':  {`.XXX.`, `X...X`, `....X`, `...X.`, `..X..`, `.X...`, `XXXXX`},
	'3':  {`XXXX.`, `....X`, `....X`, `.XXX.`, `....X`, `....X`, `XXXX.`},
	'4':  {`...X.`, `..XX.`, `.X.X.`, `X..X.`, `XXXXX`, `...X.`, `...X.`},
	'5':  {`XXXXX`, `X....`, `XXXX.`, `....X`, `....X`, `X...X`, `.XXX.`},
	'6':  {`.XXX.`, `X....`, `X....`, `XXXX.`, `X...X`, `X...X`, `.XXX.`},
	'7':  {`XXXXX`, `....X`, `...X.`, `..X..`, `.X...`, `.X...`, `.X...`},
	'8':  {`.XXX.`, `X...X`, `X...X`, `.XXX.`, `X...X`, `X...X`, `.XXX.`},
	'9':  {`.XXX.`, `X...X`, `X...X`, `.XXXX`, `....X`, `....X`, `.XXX.`},
	'.':  {`.....`, `.....`, `.....`, `.....`, `.....`, `..XX.`, `..XX.`},
	',':  {`.....`, `.....`, `.....`, `.....`, `..XX.`, `..X..`, `.X...`},
	':':  {`.....`, `..X..`, `..X..`, `.....`, `..X..`, `..X..`, `.....`},
	'!':  {`..X..`, `..X..`, `..X..`, `..X..`, `..X..`, `.....`, `..X..`},
	'?':  {`.XXX.`, `X...X`, `....X`, `...X.`, `..X..`, `.....`, `..X..`},
	'\'': {`..X..`, `..X..`, `.X...`, `.....`, `.....`, `.....`, `.....`},
	'-':  {`.....`, `.....`, `.....`, `.XXX.`, `.....`, `.....`, `.....`},
	'(':  {`...X.`, `..X..`, `.X...`, `.X...`, `.X...`, `..X..`, `...X.`},
	')':  {`.X...`, `..X..`, `...X.`, `...X.`, `...X.`, `..X..`, `.X...`},
	'/':  {`....X`, `....X`, `...X.`, `..X..`, `.X...`, `X....`, `X....`},
	'#':  {`.X.X.`, `.X.X.`, `XXXXX`, `.X.X.`, `XXXXX`, `.X.X.`, `.X.X.`},
	'+':  {`.....`, `..X..`, `..X..`, `XXXXX`, `..X..`, `..X..`, `.....`},
	'=':  {`.....`, `.....`, `XXXXX`, `.....`, `XXXXX`, `.....`, `.....`},
	'*':  {`XXXXX`, `XXXXX`, `XXXXX`, `XXXXX`, `XXXXX`, `XXXXX`, `XXXXX`},
}
