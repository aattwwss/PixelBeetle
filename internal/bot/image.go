package bot

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
)

// Image pipeline (draw-plan.md §3c): decode → box-filter downscale (fit a
// target box, aspect-preserving, never upscale) → alpha-blend over the
// empty-canvas color #1c1c1c (α < 10% skips the pixel) → weighted quantize
// to the 16-color palette. Also hosts LoadPaint, the extension dispatcher
// every input source converges on.

// LoadPaint compiles the paint input into a Blueprint. Sources:
//
//   - cfg.BlueprintPath: a .txt art file (ParseTextArt) or an image
//     (.png/.jpg/.jpeg/.gif) converted by the pipeline below
//   - cfg.Draws: -draw shape specs (shapes.go), composed after the file so
//     later specs overlay earlier art
//
// It is the single loader used by both Run (paint mode) and main.go
// (-inspect). Grid defaults (cfg.GridW/H) must already be applied.
func LoadPaint(cfg Config) (Blueprint, error) {
	var bp Blueprint
	if cfg.BlueprintPath != "" {
		data, err := os.ReadFile(cfg.BlueprintPath)
		if err != nil {
			return bp, fmt.Errorf("read %s: %w", cfg.BlueprintPath, err)
		}
		switch strings.ToLower(filepath.Ext(cfg.BlueprintPath)) {
		case ".txt":
			bp, err = ParseTextArt(data)
		case ".png", ".jpg", ".jpeg", ".gif":
			var img image.Image
			img, _, err = image.Decode(bytes.NewReader(data))
			if err == nil {
				maxW, maxH := paintTarget(cfg)
				bp, err = imageToBlueprint(img, maxW, maxH)
			}
		default:
			return bp, fmt.Errorf("unsupported blueprint file %q: want .txt, .png, .jpg, .jpeg, .gif", cfg.BlueprintPath)
		}
		if err != nil {
			return bp, err
		}
	}
	if len(cfg.Draws) > 0 {
		ps, err := ComposeShapes(cfg.Draws)
		if err != nil {
			return bp, fmt.Errorf("draw: %w", err)
		}
		bp = compose(bp, ps)
	}
	return bp, nil
}

// paintTarget is the image→blueprint box: -paint-size if set, else the grid.
func paintTarget(cfg Config) (int, int) {
	if cfg.PaintSizeSet {
		return int(cfg.PaintSize[0]), int(cfg.PaintSize[1])
	}
	return int(cfg.GridW), int(cfg.GridH)
}

// fitScale returns the destination size for a w×h image that fits within
// maxW×maxH, preserving aspect ratio and never upscaling (a smaller image
// stays at native size). Integer arithmetic only — no float drift.
func fitScale(w, h, maxW, maxH int) (int, int) {
	dw, dh := w, h
	if dw > maxW || dh > maxH {
		if dw*maxH > dh*maxW {
			dh = dh * maxW / dw
			dw = maxW
		} else {
			dw = dw * maxH / dh
			dh = maxH
		}
		if dw == 0 {
			dw = 1
		}
		if dh == 0 {
			dh = 1
		}
	}
	return dw, dh
}

const emptyCanvasRGB = 0x1c // the empty-canvas color #1c1c1c (palette.js EMPTY_RGB)

// imageToBlueprint box-filters the source image into a maximum maxW×maxH
// box. Each destination pixel averages the source pixels landing in its box
// (never a single sample, so 1px features can't vanish), then blends partial
// alpha over the empty-canvas color and quantizes to the nearest palette
// index. Fully-transparent pixels (α < ~10%) are skipped entirely.
func imageToBlueprint(img image.Image, maxW, maxH int) (Blueprint, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return Blueprint{}, fmt.Errorf("image is empty (%dx%d)", w, h)
	}
	dw, dh := fitScale(w, h, maxW, maxH)
	bp := Blueprint{W: dw, H: dh}
	for y := 0; y < dh; y++ {
		y0, y1 := y*h/dh, (y+1)*h/dh
		for x := 0; x < dw; x++ {
			x0, x1 := x*w/dw, (x+1)*w/dw
			var sr, sg, sb, sa uint64
			n := 0
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					r, g, bl, a := img.At(b.Min.X+xx, b.Min.Y+yy).RGBA()
					sr += uint64(r >> 8)
					sg += uint64(g >> 8)
					sb += uint64(bl >> 8)
					sa += uint64(a >> 8)
					n++
				}
			}
			if n == 0 {
				continue
			}
			ar, ag, ab := uint8(sr/uint64(n)), uint8(sg/uint64(n)), uint8(sb/uint64(n))
			aa := uint8(sa / uint64(n))
			if aa < 26 { // α < ~10%: leave the canvas pixel untouched
				continue
			}
			br, bg, bb := blendOverCanvas(ar, ag, ab, aa)
			bp.Placements = append(bp.Placements, Placement{X: x, Y: y, Color: nearestColor(br, bg, bb)})
		}
	}
	return bp, nil
}

// blendOverCanvas composites a premultiplied pixel over the empty-canvas
// color so partial alpha quantizes to the color it will look like once
// painted (the canvas behind it is #1c1c1c until the pixel arrives).
// image.At().RGBA() returns premultiplied channels already scaled to the
// 8-bit range after >>8, so out = src + bg·(1−α) — src needs no further
// scaling or division.
func blendOverCanvas(r, g, b, a uint8) (uint8, uint8, uint8) {
	if a == 255 {
		return r, g, b
	}
	inv := 255 - int(a)
	bg := func(v int) uint8 { return uint8(int(v) + (emptyCanvasRGB*inv+127)/255) }
	return bg(int(r)), bg(int(g)), bg(int(b))
}
