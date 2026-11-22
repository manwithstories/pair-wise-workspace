package model

import (
	"errors"
	"testing"
)

func TestTicksToNS(t *testing.T) {
	cases := []struct {
		ticks     uint64
		timescale uint32
		want      int64
	}{
		{48_000, 48_000, 1_000_000_000}, // one second of audio
		{24, 24_000, 1_000_000},         // one millisecond
		{1, 90_000, 11_111},             // rounded, no silent drift upstream
		{0, 1000, 0},
	}
	for _, c := range cases {
		got, err := TicksToNS(c.ticks, c.timescale)
		if err != nil {
			t.Fatalf("TicksToNS(%d,%d) unexpected error: %v", c.ticks, c.timescale, err)
		}
		if got != c.want {
			t.Errorf("TicksToNS(%d,%d)=%d want %d", c.ticks, c.timescale, got, c.want)
		}
	}
	if _, err := TicksToNS(1, 0); err == nil {
		t.Error("zero timescale must error")
	}
	if _, err := TicksToNS(1<<62, 1); err == nil {
		t.Error("overflow must error instead of wrapping")
	}
}

func TestMedian(t *testing.T) {
	if _, ok := Median(nil); ok {
		t.Error("empty median must report ok=false")
	}
	if v, _ := Median([]int64{5, 1, 4, 2, 3}); v != 3 {
		t.Errorf("odd median = %d, want 3", v)
	}
	if v, _ := Median([]int64{8, 1, 3, 4}); v != 3 {
		t.Errorf("even median = %d, want 3.5->3", v)
	}
}

func TestExitCode(t *testing.T) {
	if ExitCode(nil) != 0 {
		t.Error("nil error -> 0")
	}
	if ExitCode(ErrDamagedInput) != 2 || ExitCode(ErrInvalidArgs) != 2 {
		t.Error("damage/args -> 2")
	}
	if ExitCode(ErrCheckpointMismatch) != 3 || ExitCode(ErrIndexWrite) != 3 {
		t.Error("checkpoint/index -> 3")
	}
	if ExitCode(errors.New("other")) != 2 {
		t.Error("unknown error defaults to 2")
	}
}
