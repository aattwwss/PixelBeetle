package canvas

import (
	"encoding/base64"
	"fmt"
)

// Bitmap is a packed color bitmap: one byte per cell, row-major (y outer,
// x inner). Byte value 0 = empty; 1..15 = palette index (maps onto Palette in
// canvas.go). This is the in-memory + transport representation of the canvas,
// replacing per-cell DOM divs. A 1000x1000 canvas is 1MB raw and base64's to
// ~1.33MB, but compresses to a few KB when sparse (gzip/zstd).
type Bitmap struct {
	Data []byte
	W, H uint32
}

// NewBitmap allocates a zeroed bitmap of size W×H.
func NewBitmap(w, h uint32) *Bitmap {
	return &Bitmap{Data: make([]byte, w*h), W: w, H: h}
}

// Set writes a color byte at (x,y). Callers must keep x<W and y<H; no bounds
// check is performed (kept fast for the hot broadcast path).
func (b *Bitmap) Set(x, y uint32, color uint8) {
	b.Data[y*b.W+x] = color
}

// Get reads the color byte at (x,y).
func (b *Bitmap) Get(x, y uint32) uint8 {
	return b.Data[y*b.W+x]
}

// Base64 returns the standard base64 encoding of Data, used as the `bmp`
// DataStar signal payload.
func (b *Bitmap) Base64() string {
	return base64.StdEncoding.EncodeToString(b.Data)
}

// DecodeBase64 decodes a base64 string into a Bitmap of the given dimensions.
func DecodeBase64(s string, w, h uint32) (*Bitmap, error) {
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("canvas: decode bitmap: %w", err)
	}
	if uint64(len(data)) != uint64(w)*uint64(h) {
		return nil, fmt.Errorf("canvas: decoded bitmap length %d != W*H %d", len(data), uint64(w)*uint64(h))
	}
	return &Bitmap{Data: data, W: w, H: h}, nil
}
