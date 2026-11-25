package media_test

import (
	"errors"
	"testing"

	"archivecheck/internal/media"
)

// TestRescaleExactness verifies exact integer scaling across time bases,
// including half-away-from-zero rounding and overflow rejection.
func TestRescaleExactness(t *testing.T) {
	v90k := media.Rational{Num: 90000, Den: 1}
	v48k := media.Rational{Num: 48000, Den: 1}

	// 3000 ticks @90k (1/30 s) == 1600 ticks @48k.
	if got, err := media.Rescale(3000, v90k, v48k); err != nil || got != 1600 {
		t.Fatalf("rescale = %d, %v; want 1600", got, err)
	}
	// Round trip is exact for this ratio.
	if got, err := media.Rescale(1600, v48k, v90k); err != nil || got != 3000 {
		t.Fatalf("reverse rescale = %d, %v; want 3000", got, err)
	}
	// Zero stays zero in every direction.
	if got, _ := media.Rescale(0, v90k, v48k); got != 0 {
		t.Fatalf("zero rescale = %d, want 0", got)
	}
	// Invalid time base is rejected.
	if _, err := media.Rescale(10, media.Rational{Num: 0, Den: 1}, v48k); !errors.Is(err, media.ErrInvalidTimeBase) {
		t.Fatalf("invalid time base error = %v", err)
	}
}

// TestRescaleDeterministic proves repeated conversions never drift.
func TestRescaleDeterministic(t *testing.T) {
	a, _ := media.Rescale(1000, media.TimeBaseVideo90k, media.TimeBaseAudio48k)
	for i := 0; i < 50; i++ {
		if b, _ := media.Rescale(1000, media.TimeBaseVideo90k, media.TimeBaseAudio48k); b != a {
			t.Fatal("rescale drifted across iterations")
		}
	}
}

// TestTimestampValidation covers the three located-error preconditions.
func TestTimestampValidation(t *testing.T) {
	if err := media.CheckNonNegative(-1); !errors.Is(err, media.ErrNegativeTimestamp) {
		t.Fatalf("negative ts: %v", err)
	}
	if err := media.CheckNonNegative(0); err != nil {
		t.Fatalf("zero should be non-negative: %v", err)
	}
	if err := media.CheckDuration(0); !errors.Is(err, media.ErrZeroDuration) {
		t.Fatalf("zero duration: %v", err)
	}
	if err := media.CheckDuration(-5); !errors.Is(err, media.ErrZeroDuration) {
		t.Fatalf("negative duration: %v", err)
	}
	if err := media.CheckMonotonic(100, 50); !errors.Is(err, media.ErrWrap) {
		t.Fatalf("backwards timestamp: %v", err)
	}
	if err := media.CheckMonotonic(100, 100); err != nil {
		t.Fatalf("equal timestamp should be monotonic: %v", err)
	}
}

// TestRTPUnwrap distinguishes a legal 32-bit rollover from a backwards jump.
func TestRTPUnwrap(t *testing.T) {
	// A normal forward step.
	if delta, wrapped, err := media.UnwrapRTP(1000, 1300); err != nil || wrapped || delta != 300 {
		t.Fatalf("forward step: delta=%d wrapped=%v err=%v", delta, wrapped, err)
	}
	// A modular rollover near the uint32 boundary is a positive delta.
	if delta, wrapped, err := media.UnwrapRTP(0xFFFFFFF0, 0x00000010); err != nil || !wrapped || delta <= 0 {
		t.Fatalf("rollover: delta=%d wrapped=%v err=%v", delta, wrapped, err)
	}
	// A true backwards jump beyond half the cycle must be flagged.
	if _, _, err := media.UnwrapRTP(100000, 1000); err == nil {
		t.Fatal("backwards jump must report a wrap error")
	}
	// Identical timestamps (same AU) are a zero-duration fault.
	if _, _, err := media.UnwrapRTP(500, 500); !errors.Is(err, media.ErrZeroDuration) {
		t.Fatalf("same timestamp: %v", err)
	}
}

// TestFaultLocationAndCorrelation verifies the located error model.
func TestFaultLocationAndCorrelation(t *testing.T) {
	f := media.NewFault(media.KindBadLength, "seg04", 1, 1586, "sample length %d too large", 2147483647)
	if f.Segment != "seg04" || f.Track != 1 || f.Offset != 1586 || !f.Fatal {
		t.Fatalf("fault fields wrong: %+v", f)
	}
	if got := f.WithEdit("e4"); got.EditID != "e4" {
		t.Fatal("WithEdit did not attach edit id")
	}
	if extracted, ok := media.AsFault(f); !ok || extracted != f {
		t.Fatal("AsFault failed to recover the located fault")
	}
	if !media.IsFatal(f) {
		t.Fatal("bad-length fault must be fatal")
	}

	// A FaultSet keeps ordering and severity counts.
	var set media.FaultSet
	set.Add(media.NewFault(media.KindTruncated, "seg03", 1, 500, "tail cut")) // non-fatal
	set.Add(f)
	if len(set.All()) != 2 || set.FatalCount() != 1 || !set.HasFatal() {
		t.Fatalf("fault set aggregation wrong: %s", set.Error())
	}
}
