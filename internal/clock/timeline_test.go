package clock

import (
	"testing"

	"archive/internal/errs"
)

func TestRescale(t *testing.T) {
	v, err := Rescale(48000, 48000, CommonTimescale)
	if err != nil || v != 90000 {
		t.Fatalf("1s@48k -> %d, err %v", v, err)
	}
	v, err = Rescale(1024, 48000, CommonTimescale) // 21.33ms -> 1920
	if err != nil || v != 1920 {
		t.Fatalf("1024@48k -> %d, err %v", v, err)
	}
	if _, err := Rescale(1, 0, CommonTimescale); err == nil {
		t.Fatal("zero timescale accepted")
	}
}

func TestUnwrapForwardWrap(t *testing.T) {
	seq := []uint32{0xfffffff0, 0xfffffff8, 0x00000000, 0x00000008}
	got, err := UnwrapSequence(seq)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{0xfffffff0, 0xfffffff8, 0x100000000, 0x100000008}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("idx %d: got %d want %d", i, got[i], want[i])
		}
	}
}

func TestUnwrapRejectsBackwardJump(t *testing.T) {
	_, err := UnwrapSequence([]uint32{1000, 500})
	if err == nil {
		t.Fatal("backward jump accepted")
	}
	e, ok := err.(*errs.Error)
	if !ok || e.Kind != errs.KindClock {
		t.Fatalf("expected KindClock error, got %v", err)
	}
}

func TestUnwrapMonotonic(t *testing.T) {
	got, err := UnwrapSequence([]uint32{7, 7, 9, 100})
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 7 || got[3] != 100 {
		t.Fatalf("got %v", got)
	}
}
