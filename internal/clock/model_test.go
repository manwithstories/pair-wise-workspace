package clock_test

import (
	"errors"
	"testing"

	"pairwise/ingestcheck/internal/clock"
)

func TestNewRateNormalisesAndRejects(t *testing.T) {
	r, err := clock.NewRate(180000, 2)
	if err != nil {
		t.Fatalf("NewRate: %v", err)
	}
	if r.Ticks != 90000 || r.Second != 1 {
		t.Fatalf("rate not reduced: %+v", r)
	}
	for _, tc := range []struct {
		ticks, second int64
	}{
		{0, 1}, {-1, 1}, {1, 0}, {1, -1},
	} {
		if _, err := clock.NewRate(tc.ticks, tc.second); !errors.Is(err, clock.ErrInvalidRate) {
			t.Fatalf("NewRate(%d,%d) err = %v, want ErrInvalidRate", tc.ticks, tc.second, err)
		}
	}
}

func TestTicksMicrosRoundTrip(t *testing.T) {
	rate := clock.CDN()
	for _, ticks := range []int64{0, 1, 1500, 90000, 33 * 90000} {
		us, err := rate.Micros(ticks)
		if err != nil {
			t.Fatalf("Micros(%d): %v", ticks, err)
		}
		back, err := rate.TicksFor(us)
		if err != nil {
			t.Fatalf("TicksFor(%d): %v", us, err)
		}
		// Rounding down loses at most one tick.
		if d := ticks - back; d < 0 || d > 1 {
			t.Fatalf("round trip %d -> %d -> %d drifts by %d", ticks, us, back, d)
		}
	}
	if _, err := rate.TicksFor(-1); !errors.Is(err, clock.ErrNegativeDuration) {
		t.Fatalf("negative duration not rejected: %v", err)
	}
}

func TestConvertPreservesInstant(t *testing.T) {
	// One second of video at 90 kHz and audio at 48 kHz are the same instant.
	v := clock.CDN()
	a := clock.Audio()
	vt, err := clock.Convert(90000, v, v)
	if err != nil || vt != 90000 {
		t.Fatalf("identity convert = %d, %v", vt, err)
	}
	at, err := clock.Convert(90000, v, a)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if at != 48000 {
		t.Fatalf("90000 video ticks -> %d audio ticks, want 48000", at)
	}
}

func TestScaledEqual(t *testing.T) {
	ok, err := clock.ScaledEqual(90000, clock.CDN(), 48000, clock.Audio(), 0)
	if err != nil || !ok {
		t.Fatalf("exact second should match: ok=%v err=%v", ok, err)
	}
	// 10 ms of audio offset must not match a zero-tolerance comparison.
	ok, err = clock.ScaledEqual(0, clock.CDN(), 480, clock.Audio(), 0)
	if err != nil {
		t.Fatalf("scaled equal: %v", err)
	}
	if ok {
		t.Fatal("10 ms offset matched with zero tolerance")
	}
	ok, err = clock.ScaledEqual(0, clock.CDN(), 480, clock.Audio(), 10000)
	if err != nil || !ok {
		t.Fatalf("10 ms offset should match within 10 ms tolerance: ok=%v err=%v", ok, err)
	}
}

func TestTimelineRegressOpensEpoch(t *testing.T) {
	rate := clock.Audio()
	tl := clock.NewTimeline(rate)
	if tl.Epoch != 0 || tl.Pinned {
		t.Fatalf("fresh timeline not pristine: %+v", tl)
	}
	// Consume one second of wall time, then regress the media anchor.
	tl.Advance(1_000_000)
	wall := tl.Wall(48000)
	if err := tl.Regress(wall, 24000); err != nil {
		t.Fatalf("regress: %v", err)
	}
	if tl.Epoch != 1 || !tl.Pinned || tl.Span != 0 {
		t.Fatalf("regress did not open epoch: %+v", tl)
	}
	// Media ticks before the anchor still map back before the epoch start.
	if got := tl.Wall(12000); got != wall-12000 {
		t.Fatalf("pre-anchor wall = %d, want %d", got, wall-12000)
	}
	// A wall regression must be rejected, preserving the existing epoch.
	before := tl
	if err := tl.Regress(-1, 0); !errors.Is(err, clock.ErrMonotonicWall) {
		t.Fatalf("monotonic wall not enforced: %v", err)
	}
	if tl != before {
		t.Fatalf("failed regress mutated timeline: %+v vs %+v", tl, before)
	}
}

func TestErrorModelCarriesCodeOffset(t *testing.T) {
	err := clock.At(clock.CodeBoxTruncated, "mp4.parse", 1255, "box claims more bytes")
	var e *clock.Error
	if !errors.As(err, &e) {
		t.Fatalf("error is not *clock.Error: %T", err)
	}
	if e.Code != clock.CodeBoxTruncated || e.Offset != 1255 {
		t.Fatalf("unexpected code/offset: %+v", e)
	}
	if clock.CodeOf(err) != clock.CodeBoxTruncated {
		t.Fatalf("CodeOf = %v", clock.CodeOf(err))
	}
	if clock.OffsetOf(err) != 1255 {
		t.Fatalf("OffsetOf = %d", clock.OffsetOf(err))
	}
	// Wrap must preserve the underlying code and offset.
	wrapped := clock.Wrap(clock.CodeInternal, "pipeline", -1, err)
	if clock.CodeOf(wrapped) != clock.CodeBoxTruncated || clock.OffsetOf(wrapped) != 1255 {
		t.Fatalf("wrapped lost code/offset: %v", wrapped)
	}
	if msg := err.Error(); msg == "" {
		t.Fatal("empty error message")
	}
}

func TestRateOverflowRejected(t *testing.T) {
	if _, err := clock.NewRate(1<<40, 1); !errors.Is(err, clock.ErrRateOverflow) {
		t.Fatalf("oversized rate accepted: %v", err)
	}
	// A huge tick count must not silently wrap.
	r := clock.CDN()
	if _, err := r.Micros(1 << 62); !errors.Is(err, clock.ErrRateOverflow) {
		t.Fatalf("overflowing Micros accepted: %v", err)
	}
}

func TestDurationOverlap(t *testing.T) {
	a := clock.Duration{Start: 0, Len: 10}
	b := clock.Duration{Start: 5, Len: 10}
	c := clock.Duration{Start: 10, Len: 10}
	if !a.Overlaps(b) {
		t.Fatal("adjacent spans should overlap")
	}
	if a.Overlaps(c) {
		t.Fatal("touching half-open spans must not overlap")
	}
}
