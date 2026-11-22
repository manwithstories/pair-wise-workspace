package clock

import (
	"errors"
	"math"
	"testing"

	"github.com/abema/go-mp4"
)

func TestToNanosAndBack(t *testing.T) {
	tb := DefaultTimebase()
	cases := []struct{ ticks, ns int64 }{
		{0, 0},
		{1, 1_000_000},
		{500, 500_000_000},
		{-7, -7_000_000},
	}
	for _, c := range cases {
		got, err := tb.ToNanos(c.ticks)
		if err != nil {
			t.Fatalf("ToNanos(%d): %v", c.ticks, err)
		}
		if got != c.ns {
			t.Errorf("ToNanos(%d)=%d want %d", c.ticks, got, c.ns)
		}
		if back := tb.FromNanos(got); back != c.ticks {
			t.Errorf("FromNanos roundtrip=%d want %d", back, c.ticks)
		}
	}
}

func TestToNanosOverflow(t *testing.T) {
	tb := &Timebase{ScaleNS: 1 << 62}
	_, err := tb.ToNanos(8)
	if err == nil {
		t.Fatal("expected overflow error")
	}
	me, ok := AsMediaError(err)
	if !ok || me.Category != CatOverflow {
		t.Fatalf("want CatOverflow media error, got %v", err)
	}
}

func TestMp4Scale(t *testing.T) {
	ns, err := Mp4Scale(mp4.Mdhd{Timescale: 90000})
	if err != nil {
		t.Fatal(err)
	}
	if ns != 11111 { // 1e9/90000 integer division
		t.Errorf("Mp4Scale=%d want 11111", ns)
	}
	if _, err := Mp4Scale(mp4.Mdhd{}); err == nil {
		t.Error("zero timescale must error")
	}
}

func TestAlignAndTolerance(t *testing.T) {
	tb := DefaultTimebase()
	g, err := tb.Align(1_234_567_890, 500_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if g != 1_000_000_000 {
		t.Errorf("align down=%d", g)
	}
	g, _ = tb.Align(-1_234_567_890, 500_000_000)
	if g != -1_500_000_000 {
		t.Errorf("align negative=%d", g)
	}
	if !tb.WithinTolerance(1_000_000, 4_000_000) {
		t.Error("3ms apart should be within 5ms tolerance")
	}
	if tb.WithinTolerance(0, 6_000_000) {
		t.Error("6ms apart should exceed 5ms tolerance")
	}
}

func TestUnwrap16(t *testing.T) {
	// 65534 -> 1 looks like a backward jump of 65533, modularly it is +3.
	uw := Unwrap16(65534, 65534, 1)
	if uw != 65537 {
		t.Errorf("wrap forward=%d want 65537", uw)
	}
	// 2 -> 65535 is a genuine backward step of -3.
	back := Unwrap16(2, 100002, 65535)
	if back != 99999 {
		t.Errorf("wrap backward=%d want 99999", back)
	}
	// Full monotonic run across the uint16 boundary.
	var prevRaw, prevUW int64
	first := true
	var last int64
	for v := uint16(65530); ; v += 7 {
		if first {
			prevRaw, prevUW = int64(v), int64(v)
			first = false
		} else {
			prevUW = Unwrap16(prevRaw, prevUW, v)
			prevRaw = int64(v)
		}
		last = prevUW
		if v > 65530 {
			break
		}
	}
	if last <= 65535 {
		t.Errorf("unwrapped axis should cross 65535, got %d", last)
	}
}

func TestUnwrap32(t *testing.T) {
	// 4294967294 -> 3 is five modular steps forward: ...94,95,0,1,2,3.
	uw := Unwrap32(math.MaxUint32-1, math.MaxUint32-1, 3)
	if uw != math.MaxUint32+4 {
		t.Errorf("uint32 wrap=%d want %d", uw, int64(math.MaxUint32)+4)
	}
}

func TestMediaErrorWrapping(t *testing.T) {
	base := NewError(CatBadBlock, "bad header")
	me, ok := AsMediaError(base)
	if !ok {
		t.Fatal("AsMediaError failed")
	}
	if me.Offset != -1 || me.Category != CatBadBlock {
		t.Fatalf("unexpected error %+v", me)
	}
	wrapped := AtError(CatTruncated, 42, "cluster ends early")
	if !errors.Is(wrapped, wrapped) {
		t.Fatal("errors.Is identity failed")
	}
	if s := wrapped.Error(); s == "" {
		t.Fatal("empty error string")
	}
}
