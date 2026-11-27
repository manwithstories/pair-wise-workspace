package clock

import (
	"errors"
	"math"
	"testing"
)

func TestConvertRoundingAndBases(t *testing.T) {
	// 90kHz -> 1MHz：90 滴答 = 1000us
	got, err := Convert(90, 90_000, MasterTimebase)
	if err != nil || got != 1000 {
		t.Fatalf("Convert(90, 90k, 1M) = %d, %v", got, err)
	}
	// 48kHz 单滴答 -> 微秒，四舍五入得 21us
	got, err = Convert(1, 48_000, MasterTimebase)
	if err != nil || got != 21 {
		t.Fatalf("Convert(1, 48k, 1M) = %d, %v", got, err)
	}
	// 30 分钟微秒数往返 90kHz 不丢整帧
	us := int64(30 * 60 * 1_000_000)
	ticks, err := Convert(us, MasterTimebase, 90_000)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Convert(ticks, 90_000, MasterTimebase)
	if err != nil || back != us {
		t.Fatalf("round trip = %d, %v (want %d)", back, err, us)
	}
}

func TestConvertOverflow(t *testing.T) {
	_, err := Convert(math.MaxInt64, 1, MasterTimebase)
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("want ErrOverflow, got %v", err)
	}
	_, err = ConvertU(math.MaxUint64, 1, MasterTimebase)
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("unsigned want ErrOverflow, got %v", err)
	}
}

func TestUnwrapWrapsAndDupes(t *testing.T) {
	u, err := NewUnwrapper(1024)
	if err != nil {
		t.Fatal(err)
	}
	seq := []uint64{1000, 1010, 1010, 1020, 5, 15, 15, 30}
	want := []uint64{1000, 1010, 1010, 1020, 1029, 1039, 1039, 1054}
	for i, raw := range seq {
		got, err := u.Unwrap(raw)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got != want[i] {
			t.Fatalf("step %d raw=%d: got %d want %d", i, raw, got, want[i])
		}
	}
	if u.Wraps() != 1 {
		t.Fatalf("wraps = %d, want 1", u.Wraps())
	}
}

func TestUnwrapRejectsBadModulus(t *testing.T) {
	if _, err := NewUnwrapper(0); err == nil {
		t.Fatal("modulus 0 must error")
	}
	if _, err := NewUnwrapper(1000); err == nil {
		t.Fatal("non-power-of-two modulus must error")
	}
}
