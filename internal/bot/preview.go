package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"pixelbeetle/internal/canvas"
)

// previewChars maps palette index → ASCII letter. Lowercase shows EXISTING
// canvas pixels, uppercase shows what the blueprint will paint, 'X' marks an
// overpaint (blueprint pixel landing on a non-empty canvas cell).
const previewChars = "abcdefghijklmnop"

// Preview fetches the current canvas from the game server, overlays the
// blueprint at the resolved offset, and prints the result to stdout. It makes
// no claims — the whole point is to see collisions and placement BEFORE a
// multi-thousand-claim run. Image sources use their grid default exactly as
// paint mode does, so the preview never diverges from what painting will do.
func Preview(ctx context.Context, cfg Config, log *slog.Logger) error {
	bp, err := LoadPaint(cfg)
	if err != nil {
		return fmt.Errorf("preview: load: %w", err)
	}

	// The server's grid is authoritative; the -grid flag is convenience.
	body, respCode, err := getJSON(ctx, cfg.Target+"/api/canvas")
	if err != nil {
		return fmt.Errorf("preview: fetch canvas: %w", err)
	}
	if respCode != http.StatusOK {
		return fmt.Errorf("preview: fetch canvas: status %d: %s", respCode, body)
	}
	var state struct {
		GridW uint32      `json:"gridW"`
		GridH uint32      `json:"gridH"`
		Bmp   string      `json:"bmp"`
		Locks [][2]uint32 `json:"locks"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return fmt.Errorf("preview: decode canvas: %w", err)
	}

	// Server dims win: resolution + bounds validation must match the canvas
	// that will actually receive the paints.
	cfg.GridW, cfg.GridH = state.GridW, state.GridH
	off, err := resolvePaintOffset(cfg, bp)
	if err != nil {
		return fmt.Errorf("preview: %w", err)
	}
	bmp, err := canvas.DecodeBase64(state.Bmp, state.GridW, state.GridH)
	if err != nil {
		return fmt.Errorf("preview: decode bitmap: %w", err)
	}

	fmt.Printf("# %s at offset %d,%d on %dx%d canvas "+
		"(lowercase=existing, UPPERCASE=blueprint, X=overpaint, .=empty, %d locks)\n",
		cfg.BlueprintPath, off[0], off[1], state.GridW, state.GridH, len(state.Locks))
	fmt.Print(RenderPreview(bmp, state.GridW, state.GridH, bp, off))
	return nil
}

// RenderPreview renders the canvas with the blueprint overlaid at offset:
//
//	.  empty canvas cell
//	a-p  existing painted pixel (palette index → lower case letter)
//	A-P  pixel the blueprint will paint
//	X    overpaint: blueprint paints over a non-empty canvas cell
func RenderPreview(bmp *canvas.Bitmap, gw, gh uint32, bp Blueprint, off [2]uint32) string {
	cells := make(map[[2]int]uint8, len(bp.Placements))
	for _, p := range bp.Placements {
		cells[[2]int{p.X, p.Y}] = p.Color
	}
	var sb strings.Builder
	sb.Grow(int(gw) * int(gh+1))
	for y := uint32(0); y < gh; y++ {
		for x := uint32(0); x < gw; x++ {
			if c, ok := cells[[2]int{int(x) - int(off[0]), int(y) - int(off[1])}]; ok {
				if bmp.Get(x, y) == 0 {
					sb.WriteByte(previewChars[c])
				} else {
					sb.WriteByte('X')
				}
				continue
			}
			if v := bmp.Get(x, y); v == 0 {
				sb.WriteByte('.')
			} else {
				sb.WriteByte(previewChars[(v-1)%16])
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// getJSON fetches url and returns the raw body plus status code. Cheap
// helper shared by preview tooling (no struct decode here — each caller
// decodes its own shape).
func getJSON(ctx context.Context, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	// ContentLength is -1 for chunked responses — a negative cap panics.
	capHint := int64(0)
	if resp.ContentLength > 0 {
		capHint = resp.ContentLength
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, capHint+1<<20)) // hard stop at 1MiB in case the server goes rogue
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}
