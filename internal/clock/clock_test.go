package clock

import (
	"testing"

	"relay/internal/media"
)

func TestUnwrapper32(t *testing.T) {
	var u Unwrapper
	cases := []struct {
		raw  uint32
		want uint64
	}{
		{0xfffffff0, 0xfffffff0},
		{0xfffffff8, 0xfffffff8},
		{0x00000005, 0x100000005}, // forward wrap
		{0x00000009, 0x100000009},
		{0x00000007, 0x100000007}, // late packet, no re-wrap
		{0x00000010, 0x100000010},
	}
	for i, c := range cases {
		if got := u.Next(c.raw); got != c.want {
			t.Fatalf("case %d: got %d want %d", i, got, c.want)
		}
	}
}

func TestSeqUnwrapper16(t *testing.T) {
	var s SeqUnwrapper
	got := []uint64{
		s.Next(0xfffe),
		s.Next(0xffff),
		s.Next(0x0000), // wrap
		s.Next(0x0002),
		s.Next(0x0001), // reordered, keeps high bit
		s.Next(0x0003),
	}
	want := []uint64{0xfffe, 0xffff, 0x10000, 0x10002, 0x10001, 0x10003}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("case %d: got %d want %d", i, got[i], want[i])
		}
	}
}

func TestFitLine(t *testing.T) {
	// Perfectly linear observations: slope must be recovered exactly.
	pts := []Point{
		{Ticks: 0, RefNS: 1000},
		{Ticks: 10, RefNS: 2000},
		{Ticks: 20, RefNS: 3000},
		{Ticks: 30, RefNS: 4000},
	}
	f := FitLine(pts, 90000)
	if f.Slope != 100.0 {
		t.Fatalf("slope: got %v want 100", f.Slope)
	}
	if f.Intercept != 1000.0 {
		t.Fatalf("intercept: got %v want 1000", f.Intercept)
	}
}

func TestFitLineFallback(t *testing.T) {
	f := FitLine(nil, 1000)
	// Nominal slope: 1e9 ns / 1000 Hz = 1e6 ns per tick.
	if f.Slope != 1e6 {
		t.Fatalf("fallback slope: got %v want 1e6", f.Slope)
	}
}

func TestEstimatorOriginZero(t *testing.T) {
	e := NewEstimator(1000)
	e.AddAnchor(0, 5_000_000)
	e.AddAnchor(1000, 6_000_000)
	if got := e.ToNS(0); got != 0 {
		t.Fatalf("origin PTS: got %d want 0", got)
	}
	// One second of media ticks maps to one second of presentation ns.
	if got := e.ToNS(1000); got != 1_000_000 {
		t.Fatalf("1s PTS: got %d want 1000000", got)
	}
}

func TestAssignPresentationOrder(t *testing.T) {
	mk := func(src uint32, seq uint64, ticks, ref int64, hz uint64) *media.RawFrame {
		return &media.RawFrame{
			Kind: media.KindVideo,
			Meta: media.FrameMeta{
				SourceID: src, Sequence: seq, ClockTicks: uint64(ticks),
				CaptureRefNS: ref, TimescaleHz: hz,
			},
		}
	}
	// Source 1: 1000 Hz, anchors at 0,10ms,20ms,30ms. Source 2: 500 Hz at
	// the same wall clock; the OLS fit recovers each source's frame period.
	frames := []*media.RawFrame{
		mk(1, 1, 20, 20_000_000, 1000),
		mk(2, 0, 0, 0, 500),
		mk(1, 0, 0, 0, 1000),
		mk(2, 1, 10, 20_000_000, 500),
		mk(1, 2, 30, 30_000_000, 1000),
	}
	ordered := AssignPresentation(frames)
	// Expect ordering by PTS with source/sequence tie-breaking at t=0.
	type key struct {
		src uint32
		seq uint64
	}
	want := []key{{1, 0}, {2, 0}, {1, 1}, {2, 1}, {1, 2}}
	for i, w := range want {
		got := key{ordered[i].Meta.SourceID, ordered[i].Meta.Sequence}
		if got != w {
			t.Fatalf("position %d: got %+v want %+v", i, got, w)
		}
	}
	// Every PTS must be non-negative and monotonic within the merged order.
	for i, fr := range ordered {
		if fr.Meta.PresentationNS < 0 {
			t.Fatalf("negative PTS at %d: %d", i, fr.Meta.PresentationNS)
		}
		if i > 0 && ordered[i].Meta.PresentationNS < ordered[i-1].Meta.PresentationNS {
			t.Fatalf("non-monotonic PTS at %d", i)
		}
	}
}
