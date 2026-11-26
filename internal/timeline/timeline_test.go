package timeline

import (
	"errors"
	"math"
	"testing"
)

func TestNewRationalNormalizes(t *testing.T) {
	r, err := NewRational(6, 9)
	if err != nil {
		t.Fatal(err)
	}
	if r.Num() != 2 || r.Den() != 3 {
		t.Fatalf("got %s, want 2/3", r)
	}
	if _, err := NewRational(1, 0); !errors.Is(err, ErrInvalidTimebase) {
		t.Fatalf("zero denominator: %v", err)
	}
	r2, err := NewRational(-2, -4)
	if err != nil || r2.Num() != 1 || r2.Den() != 2 {
		t.Fatalf("negative normalization failed: %v %s", err, r2)
	}
}

func TestRescaleExactAndRounded(t *testing.T) {
	// 48 kHz ticks -> microseconds: 48 ticks == 1000 us.
	got, err := RescaleInt(48, Rational48k, Microsecond)
	if err != nil || got != 1000 {
		t.Fatalf("48 ticks = %d us, err=%v", got, err)
	}
	// 90 kHz: 90 ticks == 1000 us.
	got, err = RescaleInt(90, Rational90k, Microsecond)
	if err != nil || got != 1000 {
		t.Fatalf("90k ticks = %d us, err=%v", got, err)
	}
	// Round half away from zero: 1 tick at 48k = 20.833 us -> 21.
	got, _ = RescaleInt(1, Rational48k, Microsecond)
	if got != 21 {
		t.Fatalf("rounding: got %d want 21", got)
	}
	// Overflow must be reported, not wrapped.
	if _, err := RescaleInt(math.MaxInt64, MustRational(2, 1), MustRational(1, 3)); !errors.Is(err, ErrOverflow) {
		t.Fatalf("overflow not detected: %v", err)
	}
}

func TestIntervalRejectsNegative(t *testing.T) {
	if _, err := NewInterval(100, 50); !errors.Is(err, ErrNegativeDuration) {
		t.Fatalf("negative duration: %v", err)
	}
	iv, err := NewInterval(10, 90)
	if err != nil || iv.Duration() != 80 || !iv.Contains(50) || iv.Contains(90) {
		t.Fatalf("interval semantics wrong: %v %+v", err, iv)
	}
	if !iv.Overlaps(FrameInterval{50, 120}) || iv.Overlaps(FrameInterval{90, 120}) {
		t.Fatal("half-open overlap semantics wrong")
	}
}

func TestStructuredErrorModel(t *testing.T) {
	err := TrackError("op.x", 3, 90000, Rational90k, true, ErrTruncated)
	if err.Track != 3 || err.AtMicros != 1_000_000 {
		t.Fatalf("position derive failed: %+v", err)
	}
	if !errors.Is(err, ErrTruncated) || !Recoverable(err) {
		t.Fatal("sentinel match / recoverability failed")
	}
	g := GlobalError("op.g", false, ErrCorruptIndex)
	if g.Track != 0 || Recoverable(g) {
		t.Fatal("global error classification failed")
	}
	if Wrap("op", 0, 0, RationalZero, 0, false, nil) != nil {
		t.Fatal("Wrap(nil) must be nil")
	}
	if _, ok := AsError(err); !ok {
		t.Fatal("AsError failed")
	}
}

func TestUnwrapperRollover(t *testing.T) {
	u, err := NewUnwrapper(1 << 16)
	if err != nil {
		t.Fatal(err)
	}
	// Walk to the boundary then wrap.
	seq := []int64{65533, 65534, 65535, 0, 1, 2}
	want := []int64{65533, 65534, 65535, 65536, 65537, 65538}
	var sawWrap bool
	for i, raw := range seq {
		got, err := u.Expand(1, raw, RationalOne)
		if err != nil && errors.Is(err, ErrWrap) {
			sawWrap = true
		} else if err != nil {
			t.Fatalf("expand %d: %v", raw, err)
		}
		if got != want[i] {
			t.Fatalf("raw %d -> %d, want %d", raw, got, want[i])
		}
	}
	if !sawWrap || u.Wraps() != 1 {
		t.Fatalf("wrap not recorded: %+v", u)
	}
}

func TestUnwrapperDuplicatesAndJump(t *testing.T) {
	u, _ := NewUnwrapper(1000)
	// 300 -> 200 is a -100 jump, smaller than half the modulus (500): a real
	// backwards discontinuity rather than a modulus rollover.
	vals := []int64{100, 100, 101, 300, 200}
	var expanded []int64
	var jumpErr error
	for _, v := range vals {
		got, err := u.Expand(2, v, RationalOne)
		if err != nil && errors.Is(err, ErrWrap) && u.Backwards() > 0 {
			jumpErr = err
		} else if err != nil {
			t.Fatal(err)
		}
		expanded = append(expanded, got)
	}
	if expanded[0] != expanded[1] || expanded[2] != 101 {
		t.Fatalf("duplicate handling: %v", expanded)
	}
	if jumpErr == nil || u.Backwards() != 1 || !Recoverable(jumpErr) {
		t.Fatalf("backwards jump not flagged: %v %+v", jumpErr, u)
	}
	u.Reset()
	if _, ok := u.Last(); ok {
		t.Fatal("reset did not clear state")
	}
}

func TestUnwrapperBadModulus(t *testing.T) {
	if _, err := NewUnwrapper(0); !errors.Is(err, ErrInvalidTimebase) {
		t.Fatalf("modulus check: %v", err)
	}
}
