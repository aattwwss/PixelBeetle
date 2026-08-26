// Package warm extracts posted-claim facts from raw TigerBeetle transfers.
// The pixel cache and the time-travel manifest are built in game.Service.WarmCache
// (which inlines the page-and-fold so it can derive both in one pass); this
// package holds the shared predicate for recognizing a posted claim leg.
package warm

import (
	tb "github.com/tigerbeetle/tigerbeetle-go"

	"pixelbeetle/internal/tbclient"
)

// PostedClaim extracts (x, y, color) from a posted claim leg. ok is false
// when the transfer isn't a posted claim for an in-bounds pixel. A posted
// claim leg has Flags bit 2 (post_pending_transfer); its debit account is the
// pixel and its code is TransferCodeClaim | color.
func PostedClaim(t tb.Transfer, gridW, gridH uint32) (x, y uint32, color uint8, ok bool) {
	if t.Flags&0x4 == 0 {
		return 0, 0, 0, false
	}
	x, y, ok = tbclient.UnpackPixelID(t.DebitAccountID)
	if !ok || x >= gridW || y >= gridH {
		return 0, 0, 0, false
	}
	if t.Code < tbclient.TransferCodeClaim {
		return 0, 0, 0, false
	}
	return x, y, uint8(t.Code - tbclient.TransferCodeClaim), true
}
