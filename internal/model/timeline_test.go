package model

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestScaleExactAndNegative(t *testing.T) {
	cases := []struct {
		ticks    int64
		from, to uint32
		want     int64
	}{
		{90000, 90000, 1_000_000, 1_000_000},
		{45000, 90000, 1_000_000, 500_000},
		{48000, 48000, 1_000_000, 1_000_000},
		{-90000, 90000, 1_000_000, -1_000_000},
		{0, 90000, 1_000_000, 0},
		{1, 1, 1_000_000, 1_000_000},
		{1, 1_000_000, 1, 0},  // half tick truncates toward zero
		{2, 1_000_000, 1, 0},  // fractional remainder is dropped
		{3, 1_000_000, 2, 0},  // still below one tick
		{-3, 1_000_000, 1, 0}, // negative truncation is toward zero
	}
	for _, c := range cases {
		got, ok := Scale(c.ticks, c.from, c.to)
		if !ok || got != c.want {
			t.Errorf("Scale(%d,%d,%d) = %d,%v want %d", c.ticks, c.from, c.to, got, ok, c.want)
		}
	}
}

func TestScaleRejectsOverflow(t *testing.T) {
	if v, ok := Scale(math.MaxInt64, 1_000_000, 2_000_000); ok {
		t.Fatalf("expected overflow, got %d", v)
	}
	if v, ok := Scale(math.MinInt64, 1, 1_000_000); ok {
		t.Fatalf("expected overflow for min, got %d", v)
	}
	if v, ok := Scale(1, 0, 1); ok {
		t.Fatalf("expected failure for zero source scale, got %d", v)
	}
	// Boundary: exactly representable maximum.
	max, ok := Scale(math.MaxInt64, 1_000_000, 1_000_000)
	if !ok || max != math.MaxInt64 {
		t.Fatalf("identity max should hold, got %d %v", max, ok)
	}
}

func TestTimebaseNonZeroOrigin(t *testing.T) {
	tb := Timebase{TrackID: 1, Timescale: 90000, OriginTicks: 90_000 * 3600}
	u, err := tb.ToUnified(tb.OriginTicks)
	if err != nil {
		t.Fatal(err)
	}
	if u != 3600*1_000_000 {
		t.Fatalf("origin unified = %d", u)
	}
	// One frame later at 25 fps.
	u2, _ := tb.ToUnified(tb.OriginTicks + 3600)
	if u2-u != 40000 {
		t.Fatalf("frame delta = %d", u2-u)
	}
}

func TestTimebaseOverflowCarriesCode(t *testing.T) {
	tb := Timebase{TrackID: 7, Timescale: 1, OriginTicks: 0}
	_, err := tb.ToUnified(math.MaxInt64)
	if CodeOf(err) != CodeTimescaleOverflow {
		t.Fatalf("code = %q, err = %v", CodeOf(err), err)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatal("not a *model.Error")
	}
	if e.Track != 7 {
		t.Errorf("track = %d", e.Track)
	}
}

func TestTimelineRejectsBadTracks(t *testing.T) {
	if _, err := NewTimeline(Timebase{TrackID: 1, Timescale: 0}); CodeOf(err) != CodeTimescaleOverflow {
		t.Fatalf("zero timescale code = %q", CodeOf(err))
	}
	if _, err := NewTimeline(Timebase{TrackID: 0, Timescale: 1000}); CodeOf(err) != CodeManifestReference {
		t.Fatalf("track 0 code = %q", CodeOf(err))
	}
	if _, err := NewTimeline(
		Timebase{TrackID: 2, Timescale: 1000},
		Timebase{TrackID: 2, Timescale: 1000},
	); CodeOf(err) != CodeManifestReference {
		t.Fatalf("duplicate code = %q", CodeOf(err))
	}
}

func TestTimelineLookup(t *testing.T) {
	tl, err := NewTimeline(Timebase{TrackID: 1, Timescale: 90000})
	if err != nil {
		t.Fatal(err)
	}
	u, err := tl.Unified(1, 90000)
	if err != nil || u != 1_000_000 {
		t.Fatalf("unified = %d %v", u, err)
	}
	if _, err := tl.Unified(9, 1); CodeOf(err) != CodeManifestReference {
		t.Fatalf("missing track code = %q", CodeOf(err))
	}
}

func TestToDuration(t *testing.T) {
	d, ok := ToDuration(3_600_000_000)
	if !ok || d != time.Hour {
		t.Fatalf("duration = %v %v", d, ok)
	}
	if _, ok := ToDuration(math.MaxInt64); ok {
		t.Fatal("expected duration overflow")
	}
	if FromDuration(time.Hour) != 3_600_000_000 {
		t.Fatal("round trip failed")
	}
}

func TestErrorFormattingIncludesLocation(t *testing.T) {
	e := &Error{Code: CodeTruncatedSample, File: "clip.mp4", Line: 12, Track: 3, Sample: 44,
		OffsetBytes: 184320, Detail: "expected 120 bytes"}
	s := e.Error()
	for _, want := range []string{"truncated_sample", "line 12", "track 3", "sample 44", "offset 184320"} {
		if !contains(s, want) {
			t.Errorf("missing %q in %q", want, s)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
