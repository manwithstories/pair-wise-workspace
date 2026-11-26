package media

import (
	"errors"
	"testing"
)

func TestRationalNormalizationAndCompare(t *testing.T) {
	r, err := NewRational(4, 6)
	if err != nil {
		t.Fatal(err)
	}
	if r != (Rational{Num: 2, Den: 3}) {
		t.Fatalf("unexpected normalization: %+v", r)
	}
	if got := MustRational(1, 2).Cmp(MustRational(2, 4)); got != 0 {
		t.Fatalf("equivalent rationals compare = %d", got)
	}
	if got := MustRational(1, 3).Cmp(MustRational(1, 2)); got != -1 {
		t.Fatalf("1/3 vs 1/2 = %d, want -1", got)
	}
	if _, err := NewRational(1, 0); err == nil {
		t.Fatal("expected zero denominator error")
	}
}

func TestClockConvertsExactCommonTimescales(t *testing.T) {
	video, err := NewClock(90_000)
	if err != nil {
		t.Fatal(err)
	}
	// One 30 fps frame is 3000 ticks and must be 33,333,333 ns after rounding.
	ns, err := video.TicksToNanoseconds(3000)
	if err != nil {
		t.Fatal(err)
	}
	if ns != 33_333_333 {
		t.Fatalf("frame ns = %d", ns)
	}
	ticks, err := video.NanosecondsToTicks(1_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if ticks != 90_000 {
		t.Fatalf("one second ticks = %d", ticks)
	}

	audio, err := NewClock(48_000)
	if err != nil {
		t.Fatal(err)
	}
	converted, err := ConvertTicks(48_000, audio, video)
	if err != nil {
		t.Fatal(err)
	}
	if converted != 90_000 {
		t.Fatalf("converted = %d, want 90000", converted)
	}
}

func TestClockRejectsZeroTimescaleAndOverflow(t *testing.T) {
	if _, err := NewClock(0); err == nil {
		t.Fatal("expected zero timescale rejection")
	}
	clock := Clock{SecondsPerTick: MustRational(2, 1_000_000_000)}
	if _, err := clock.TicksToNanoseconds(1 << 62); err == nil {
		t.Fatal("expected overflow error")
	}
}

func TestFaultContainsLocationAndSupportsErrorsAs(t *testing.T) {
	cause := errors.New("boom")
	f := NewFault(CodeBadSampleOffset, "offset points outside mdat").
		WithTrack(7).
		WithBox("stco").
		WithByteRange(1234, 4).
		WithOriginal("offset=999999").
		WithBasis("contiguous sample layout").
		Fatal()
	f.Cause = cause
	err := error(f)
	got, ok := AsFault(err)
	if !ok || got.TrackID != 7 || got.ByteRange.Offset != 1234 {
		t.Fatalf("fault extraction failed: %#v %v", got, ok)
	}
	if !errors.Is(err, cause) {
		t.Fatal("fault does not unwrap cause")
	}
	if f.Recoverable {
		t.Fatal("Fatal did not mark fault non-recoverable")
	}
}
