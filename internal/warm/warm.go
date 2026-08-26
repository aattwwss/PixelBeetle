// Package warm rebuilds the in-memory pixel cache from TigerBeetle transfer
// history. The fold reads only posted claim legs (Flags bit 2), whose code is
// the pending claim's code (color) and whose debit account is the pixel.
package warm

import (
	"log/slog"
	"sort"

	tb "github.com/tigerbeetle/tigerbeetle-go"

	"pixelbeetle/internal/tbclient"
)

// Pixel is a painted cell as derived from transfer history.
type Pixel struct {
	X, Y    uint32
	Color   uint8
	Version uint64 // number of times this pixel was posted (painted)
}

// Scan pages through all canvas-ledger transfers and folds them into pixels.
func Scan(client *tbclient.Client, gridW, gridH uint32, log *slog.Logger) ([]Pixel, error) {
	const limit = 4000

	var all []tb.Transfer
	var from uint64
	for {
		page, err := client.QueryCanvasTransfers(from, limit)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < limit {
			break
		}
		// Timestamps are unique per transfer; inclusive bounds mean we resume
		// strictly after the last one we already saw.
		from = page[len(page)-1].Timestamp + 1
	}

	pixels := Fold(all, gridW, gridH)
	log.Debug("warm scan complete", "transfers", len(all), "pixels", len(pixels))
	return pixels, nil
}

// Fold reduces transfers (ascending by timestamp) to the final pixel states.
// A posted claim leg has Flags bit 2 (post_pending_transfer) set; its debit
// account is the pixel and its code is TransferCodeClaim | color (inherited
// from the pending claim). Everything else — pending claims, voids, and the
// single-phase fund/refund transfers — is ignored.
func Fold(transfers []tb.Transfer, gridW, gridH uint32) []Pixel {
	seen := make(map[uint64]Pixel)
	for _, t := range transfers {
		if t.Flags&0x4 == 0 {
			continue
		}
		x, y, ok := tbclient.UnpackPixelID(t.DebitAccountID)
		if !ok || x >= gridW || y >= gridH {
			continue
		}
		if t.Code < tbclient.TransferCodeClaim {
			continue
		}
		key := uint64(x)<<32 | uint64(y)
		prev := seen[key]
		seen[key] = Pixel{X: x, Y: y, Color: uint8(t.Code - tbclient.TransferCodeClaim), Version: prev.Version + 1}
	}

	out := make([]Pixel, 0, len(seen))
	for _, p := range seen {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		ki := uint64(out[i].X)<<32 | uint64(out[i].Y)
		kj := uint64(out[j].X)<<32 | uint64(out[j].Y)
		return ki < kj
	})
	return out
}
