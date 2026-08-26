package warm

import (
	"testing"

	tb "github.com/tigerbeetle/tigerbeetle-go"

	"pixelbeetle/internal/tbclient"
)

func TestFoldPaintsPostedClaims(t *testing.T) {
	transfers := []tb.Transfer{
		// A posted claim on (3,4) with color 7 (code 1007).
		{DebitAccountID: tbclient.PixelID(3, 4), Code: 1007, Flags: 0x5, Timestamp: 1},
	}
	got := Fold(transfers, 64, 64)
	if len(got) != 1 {
		t.Fatalf("want 1 pixel, got %d", len(got))
	}
	p := got[0]
	if p.X != 3 || p.Y != 4 || p.Color != 7 || p.Version != 1 {
		t.Fatalf("unexpected pixel: %+v", p)
	}
}

func TestFoldLastWriteWinsAndCountsVersions(t *testing.T) {
	transfers := []tb.Transfer{
		{DebitAccountID: tbclient.PixelID(3, 4), Code: 1002, Flags: 0x5, Timestamp: 1},
		{DebitAccountID: tbclient.PixelID(3, 4), Code: 1009, Flags: 0x5, Timestamp: 2},
	}
	got := Fold(transfers, 64, 64)
	if len(got) != 1 {
		t.Fatalf("want 1 pixel, got %d", len(got))
	}
	if got[0].Color != 9 || got[0].Version != 2 {
		t.Fatalf("want color 9 version 2, got %+v", got[0])
	}
}

func TestFoldIgnoresNonPostLegs(t *testing.T) {
	transfers := []tb.Transfer{
		{DebitAccountID: tbclient.PixelID(1, 1), Code: 1005, Flags: 0x2, Timestamp: 1}, // pending
		{DebitAccountID: tbclient.PixelID(2, 2), Code: 1006, Flags: 0x8, Timestamp: 2}, // void
		{DebitAccountID: tbclient.PixelID(3, 3), Code: 1001, Flags: 0x0, Timestamp: 3}, // fund/refund
	}
	if got := Fold(transfers, 64, 64); len(got) != 0 {
		t.Fatalf("want 0 pixels, got %d", len(got))
	}
}

func TestFoldSkipsOutOfBoundsAndForeignIDs(t *testing.T) {
	transfers := []tb.Transfer{
		{DebitAccountID: tbclient.PixelID(64, 0), Code: 1001, Flags: 0x5, Timestamp: 1}, // x == gridW
		{DebitAccountID: tbclient.PixelID(0, 64), Code: 1001, Flags: 0x5, Timestamp: 2}, // y == gridH
		{DebitAccountID: tb.ToUint128(5), Code: 1001, Flags: 0x5, Timestamp: 3},         // not a pixel id
		{DebitAccountID: tbclient.PixelID(7, 7), Code: 999, Flags: 0x5, Timestamp: 4},   // code below claim base
	}
	if got := Fold(transfers, 64, 64); len(got) != 0 {
		t.Fatalf("want 0 pixels, got %d", len(got))
	}
}
