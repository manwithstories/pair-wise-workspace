package media

import (
	"math"
	"testing"
)

func TestConvert90kCommonTimescales(t *testing.T) {
	cases := []struct {
		value     uint64
		timescale uint32
		want      int64
	}{
		{180000, 90000, 180000},
		{96000, 48000, 180000},
		{88200, 44100, 180000},
		{0, 90000, 0},
	}
	for _, tc := range cases {
		got, err := Convert90k(tc.value, tc.timescale)
		if err != nil {
			t.Fatalf("Convert90k(%d,%d): %v", tc.value, tc.timescale, err)
		}
		if got != tc.want {
			t.Fatalf("Convert90k(%d,%d)=%d want %d", tc.value, tc.timescale, got, tc.want)
		}
	}
}

func TestConvert90kRejectsZeroAndOverflow(t *testing.T) {
	if _, err := Convert90k(1, 0); err == nil {
		t.Fatal("zero timescale accepted")
	}
	if _, err := Convert90k(math.MaxUint64, 1); err == nil {
		t.Fatal("overflowing conversion accepted")
	}
}

func TestWithinOneTick(t *testing.T) {
	if !WithinOneTick(10, 11) {
		t.Fatal("one-tick difference must be tolerated")
	}
	if WithinOneTick(10, 12) {
		t.Fatal("two-tick difference must not be tolerated")
	}
}
