package canvas

import "testing"

func TestNewBitmapZeroed(t *testing.T) {
	b := NewBitmap(4, 3)
	if b.W != 4 || b.H != 3 {
		t.Fatalf("dims = %dx%d, want 4x3", b.W, b.H)
	}
	if len(b.Data) != 12 {
		t.Fatalf("data len = %d, want 12", len(b.Data))
	}
	for i, v := range b.Data {
		if v != 0 {
			t.Fatalf("data[%d] = %d, want 0", i, v)
		}
	}
}

func TestSetGetRoundTrip(t *testing.T) {
	b := NewBitmap(8, 8)
	cases := []struct {
		x, y uint32
		c    uint8
	}{
		{0, 0, 1},
		{7, 0, 2},  // top-right
		{0, 7, 3},  // bottom-left
		{7, 7, 4},  // bottom-right
		{3, 4, 15}, // center-ish
		{0, 0, 0},  // overwrite with empty
	}
	for _, c := range cases {
		b.Set(c.x, c.y, c.c)
		if got := b.Get(c.x, c.y); got != c.c {
			t.Errorf("Get(%d,%d) = %d, want %d", c.x, c.y, got, c.c)
		}
	}
}

func TestBase64DecodeRoundTrip(t *testing.T) {
	b := NewBitmap(5, 5)
	b.Set(0, 0, 1)
	b.Set(4, 4, 9)
	b.Set(2, 2, 15)

	enc := b.Base64()
	got, err := DecodeBase64(enc, 5, 5)
	if err != nil {
		t.Fatalf("DecodeBase64: %v", err)
	}
	if len(got.Data) != 25 {
		t.Fatalf("decoded len = %d, want 25", len(got.Data))
	}
	for i := range b.Data {
		if got.Data[i] != b.Data[i] {
			t.Fatalf("data[%d] = %d, want %d", i, got.Data[i], b.Data[i])
		}
	}
}

func TestDecodeBase64WrongLength(t *testing.T) {
	b := NewBitmap(2, 2)
	if _, err := DecodeBase64(b.Base64(), 3, 3); err == nil {
		t.Fatal("expected error for mismatched W*H")
	}
}
