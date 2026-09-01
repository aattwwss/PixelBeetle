package tbclient

import (
	"testing"

	"github.com/google/uuid"
	tb "github.com/tigerbeetle/tigerbeetle-go"
)

func TestIsPostedClaimRecognizesPostLeg(t *testing.T) {
	x, y, color, ok := IsPostedClaim(tb.Transfer{
		DebitAccountID: PixelID(3, 4),
		Code:           1007, // TransferCodeClaim | 7
		Flags:          0x5,  // post_pending_transfer (bit 2) set
	}, 64, 64)
	if !ok || x != 3 || y != 4 || color != 7 {
		t.Fatalf("got (%d,%d,%d,%v), want (3,4,7,true)", x, y, color, ok)
	}
}

func TestIsPostedClaimIgnoresNonPostLegs(t *testing.T) {
	cases := []struct {
		name  string
		flags uint16
	}{
		{"pending", 0x2},
		{"void", 0x8},
		{"fund", 0x0},
	}
	for _, c := range cases {
		if _, _, _, ok := IsPostedClaim(tb.Transfer{
			DebitAccountID: PixelID(1, 1),
			Code:           1005,
			Flags:          c.flags,
		}, 64, 64); ok {
			t.Errorf("%s: expected ok=false", c.name)
		}
	}
}

func TestIsPostedClaimSkipsOutOfBoundsAndForeign(t *testing.T) {
	cases := []struct {
		name string
		id   tb.Uint128
		code uint16
	}{
		{"x == gridW", PixelID(64, 0), 1001},
		{"y == gridH", PixelID(0, 64), 1001},
		{"not a pixel id", tb.ToUint128(5), 1001},
		{"code below claim base", PixelID(7, 7), 999},
	}
	for _, c := range cases {
		if _, _, _, ok := IsPostedClaim(tb.Transfer{
			DebitAccountID: c.id,
			Code:           c.code,
			Flags:          0x5,
		}, 64, 64); ok {
			t.Errorf("%s: expected ok=false", c.name)
		}
	}
}

func TestNewClaimClampsColor(t *testing.T) {
	tc := NewClaim(0, 0, 200, uuid.New())
	if got := tc.Code; got != TransferCodeClaim+uint16(MaxColor) {
		t.Fatalf("code %d, want %d (clamped to MaxColor)", got, TransferCodeClaim+uint16(MaxColor))
	}
	tc = NewClaim(0, 0, MaxColor, uuid.New())
	if got := tc.Code; got != TransferCodeClaim+uint16(MaxColor) {
		t.Fatalf("code %d, want %d (MaxColor unchanged)", got, TransferCodeClaim+uint16(MaxColor))
	}
}
