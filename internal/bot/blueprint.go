package bot

import (
	"fmt"
	"sort"
	"strings"
)

// Placement is one pixel of a drawing: a palette index (0-15) at
// (X, Y) relative to the blueprint's top-left corner.
type Placement struct {
	X, Y  int
	Color uint8
}

// Blueprint is the intermediate representation everything the bot paints is
// compiled to. All sources (text art files now; shape primitives and images
// in later phases) converge on this type. Placements are in scanline order
// (top row left-to-right, then next row) unless reordered for painting.
type Blueprint struct {
	W, H       int
	Placements []Placement
}

// ParseTextArt parses the 3a text-art format from draw-plan.md:
//
//	legend: k=#222222 w=#ffffff ...
//	  ####
//	 #    #
//	 ...
//
// Rules (complete):
//   - Optional line 1: `legend:` mapping printable chars to #rrggbb hex.
//     `.` and space always mean SKIP (leave the canvas pixel untouched).
//   - Ragged lines are fine (a short row means the rest of that row is skip).
//   - No leading blank lines; blank lines inside the art are skip-rows.
//   - Any rune with no legend entry (other than `.`/space) is a fatal error
//     with a 1-based line/column position — no guessing.
//   - Parsed as runes, not bytes (multi-byte legend characters cannot
//     misalign columns).
func ParseTextArt(data []byte) (Blueprint, error) {
	var bp Blueprint
	raw := strings.Split(string(data), "\n")
	if n := len(raw); n > 0 && raw[n-1] == "" {
		raw = raw[:n-1] // trailing newline
	}

	lineOffset := 0 // 1-based number of the first raw line that is art
	legend := map[rune]uint8{}
	if len(raw) > 0 && strings.HasPrefix(raw[0], "legend:") {
		if err := parseLegend(raw[0], legend); err != nil {
			return bp, parseErr(1, 0, err)
		}
		raw = raw[1:]
		lineOffset = 1
	}

	for li, line := range raw {
		line = strings.TrimSuffix(line, "\r") // Windows line endings
		x := 0
		for _, r := range line {
			switch {
			case r == '.' || r == ' ':
				x++
				continue
			default:
				idx, ok := legend[r]
				if !ok {
					return bp, fmt.Errorf("blueprint line %d col %d: char %q has no legend entry",
						li+1+lineOffset, x+1, r)
				}
				bp.Placements = append(bp.Placements, Placement{X: x, Y: li, Color: idx})
				x++
			}
		}
		if x > bp.W {
			bp.W = x
		}
	}
	bp.H = len(raw)
	return bp, nil
}

func parseErr(line int, _ uint8, err error) error {
	return fmt.Errorf("blueprint line %d: %w", line, err)
}

// parseLegend scans "legend: c=#hex c=#hex ..." after the "legend:" prefix.
// '.' and space are reserved for skip and cannot be legend characters.
func parseLegend(line string, legend map[rune]uint8) error {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "legend:"))
	for _, tok := range strings.Fields(rest) {
		eq := strings.IndexByte(tok, '=')
		if eq != 1 || eq == len(tok)-1 {
			return fmt.Errorf("bad legend token %q, want c=#rrggbb", tok)
		}
		r := rune(tok[0])
		if r == '.' || r == ' ' {
			return fmt.Errorf("legend char %q is reserved for skip", tok[0])
		}
		idx, err := PaletteColor(tok[eq+1:])
		if err != nil {
			return fmt.Errorf("legend char %q: %w", tok[0], err)
		}
		legend[r] = idx
	}
	return nil
}

// compose merges placements into one blueprint, keeping the LAST placement at
// any shared cell (later -draw specs overlay earlier art), and restoring
// scanline order. Cells not covered by any placement stay canvas (skipped).
func compose(base Blueprint, more []Placement) Blueprint {
	cells := map[[2]int]uint8{}
	w, h := base.W, base.H
	for _, p := range base.Placements {
		cells[[2]int{p.X, p.Y}] = p.Color
		if p.X+1 > w {
			w = p.X + 1
		}
		if p.Y+1 > h {
			h = p.Y + 1
		}
	}
	for _, p := range more {
		cells[[2]int{p.X, p.Y}] = p.Color
		if p.X+1 > w {
			w = p.X + 1
		}
		if p.Y+1 > h {
			h = p.Y + 1
		}
	}
	bp := Blueprint{W: w, H: h}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if c, ok := cells[[2]int{x, y}]; ok {
				bp.Placements = append(bp.Placements, Placement{X: x, Y: y, Color: c})
			}
		}
	}
	return bp
}

// FormatTextArt renders a blueprint back to the 3a text-art format (legend
// + rows) — the -inspect output for images, and the round-trip format
// images are reviewed in. Legend chars are assigned by color usage: most-
// used color → 'a', next → 'b', ... so a compact single-char legend. Cells
// covered by no placement render as '.'. The output parses back cleanly
// with ParseTextArt (tested).
func FormatTextArt(bp Blueprint) string {
	cells := map[[2]int]uint8{}
	for _, p := range bp.Placements {
		cells[[2]int{p.X, p.Y}] = p.Color
	}
	var counts [16]int
	for _, c := range cells {
		counts[c]++
	}
	order := make([]int, 0, 16)
	for i, n := range counts {
		if n > 0 {
			order = append(order, i)
		}
	}
	sort.Slice(order, func(a, b int) bool {
		if counts[order[a]] != counts[order[b]] {
			return counts[order[a]] > counts[order[b]]
		}
		return order[a] < order[b]
	})
	const chars = "abcdefghijklmnop"
	var charOf [16]byte
	for i, c := range order {
		charOf[c] = chars[i]
	}

	var sb strings.Builder
	sb.WriteString("legend:")
	for _, c := range order {
		p := palette[c]
		fmt.Fprintf(&sb, " %c=#%02x%02x%02x", charOf[c], p[0], p[1], p[2])
	}
	sb.WriteByte('\n')
	for y := 0; y < bp.H; y++ {
		for x := 0; x < bp.W; x++ {
			if c, ok := cells[[2]int{x, y}]; ok {
				sb.WriteByte(charOf[c])
			} else {
				sb.WriteByte('.')
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// ValidateBounds checks the blueprint at the given top-left offset fits the
// grid. Paint mode calls this before any claim is submitted — a drawing that
// runs off the canvas is an authoring error, not something to clip silently.
func (b Blueprint) ValidateBounds(gridW, gridH uint32, offset [2]uint32) error {
	if b.W+int(offset[0]) > int(gridW) || b.H+int(offset[1]) > int(gridH) {
		return fmt.Errorf("blueprint %dx%d at offset %d,%d exceeds grid %dx%d",
			b.W, b.H, offset[0], offset[1], gridW, gridH)
	}
	return nil
}
