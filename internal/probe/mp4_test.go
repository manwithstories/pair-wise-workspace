package probe

import (
	"testing"
)

func TestParseClap(t *testing.T) {
	// 1920x1080 coded, clean 1912x1072 centred with no offset.
	buf := make([]byte, 32)
	vals := []int32{1912, 1, 1072, 1, 0, 1, 0, 1}
	for i, v := range vals {
		buf[i*4] = byte(uint32(v) >> 24)
		buf[i*4+1] = byte(uint32(v) >> 16)
		buf[i*4+2] = byte(uint32(v) >> 8)
		buf[i*4+3] = byte(v)
	}
	c, err := parseClap(buf)
	if err != nil {
		t.Fatal(err)
	}
	crop, err := c.crop(1920, 1080)
	if err != nil {
		t.Fatal(err)
	}
	if crop.Left != 4 || crop.Right != 4 || crop.Top != 4 || crop.Bottom != 4 {
		t.Fatalf("crop=%+v", crop)
	}
	if w, _ := crop.Width(1920); w != 1912 {
		t.Fatalf("clean width=%d", w)
	}
}

func TestParseClapRejectsZeroDenominator(t *testing.T) {
	buf := make([]byte, 32)
	buf[7] = 1 // width denominator 0? numerator 0/0
	if _, err := parseClap(buf); err == nil {
		t.Fatal("expected denominator error")
	}
	if _, err := parseClap(make([]byte, 8)); err == nil {
		t.Fatal("expected length error")
	}
}

func TestRotationFromMatrix(t *testing.T) {
	id := [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}
	cases := []struct {
		m    [9]int32
		want int
	}{
		{id, 0},
		{[9]int32{0, 0x10000, 0, -0x10000, 0, 0, 0, 0, 0x40000000}, 90},
		{[9]int32{-0x10000, 0, 0, 0, -0x10000, 0, 0, 0, 0x40000000}, 180},
		{[9]int32{0, -0x10000, 0, 0x10000, 0, 0, 0, 0, 0x40000000}, 270},
	}
	for _, c := range cases {
		got, err := rotationFromMatrix(c.m)
		if err != nil {
			t.Fatalf("matrix %v: %v", c.m, err)
		}
		if got != c.want {
			t.Errorf("matrix %v -> %d want %d", c.m, got, c.want)
		}
	}
}

func TestRotationRejectsMirrorAndSkew(t *testing.T) {
	if _, err := rotationFromMatrix([9]int32{-0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}); err == nil {
		t.Fatal("mirror should be rejected")
	}
	if _, err := rotationFromMatrix([9]int32{0x10000, 0x8000, 0, 0, 0x10000, 0, 0, 0, 0x40000000}); err == nil {
		t.Fatal("skew should be rejected")
	}
	if _, err := rotationFromMatrix([9]int32{0, 0, 0, 0, 0, 0, 0, 0, 0x40000000}); err == nil {
		t.Fatal("singular matrix should be rejected")
	}
}
