package warm

import (
	"testing"

	tb "github.com/tigerbeetle/tigerbeetle-go"

	"pixelbeetle/internal/tbclient"
)

func TestPostedClaimRecognizesPostLeg(t *testing.T) {
	x, y, color, ok := PostedClaim(tb.Transfer{
		DebitAccountID: tbclient.PixelID(3, 4),
		Code:           1007, // TransferCodeClaim | 7
		Flags:          0x5,  // post_pending_transfer (bit 2) set
	}, 64, 64)
	if !ok || x != 3 || y != 4 || color != 7 {
		t.Fatalf("got (%d,%d,%d,%v), want (3,4,7,true)", x, y, color, ok)
	}
}

func TestPostedClaimIgnoresNonPostLegs(t *testing.T) {
	cases := []struct {
		name  string
		flags uint16
	}{
		{"pending", 0x2},
		{"void", 0x8},
		{"fund", 0x0},
	}
	for _, c := range cases {
		if _, _, _, ok := PostedClaim(tb.Transfer{
			DebitAccountID: tbclient.PixelID(1, 1),
			Code:           1005,
			Flags:          c.flags,
		}, 64, 64); ok {
			t.Errorf("%s: expected ok=false", c.name)
		}
	}
}

func TestPostedClaimSkipsOutOfBoundsAndForeign(t *testing.T) {
	cases := []struct {
		name string
		id   tb.Uint128
		code uint16
	}{
		{"x == gridW", tbclient.PixelID(64, 0), 1001},
		{"y == gridH", tbclient.PixelID(0, 64), 1001},
		{"not a pixel id", tb.ToUint128(5), 1001},
		{"code below claim base", tbclient.PixelID(7, 7), 999},
	}
	for _, c := range cases {
		if _, _, _, ok := PostedClaim(tb.Transfer{
			DebitAccountID: c.id,
			Code:           c.code,
			Flags:          0x5,
		}, 64, 64); ok {
			t.Errorf("%s: expected ok=false", c.name)
		}
	}
}
