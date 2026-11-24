package media

import (
	"errors"
	"testing"
)

func TestFromScaledTimecode(t *testing.T) {
	cases := []struct {
		name     string
		scale    uint64
		timecode int64
		want     MediaTime
	}{
		{"default ms scale", DefaultTimestampScale, 250, FromMilli(250)},
		{"custom 100ns units", 100, 7, FromNanoseconds(700)},
		{"zero scale falls back", 0, 3, FromMilli(3)},
		{"negative timecode", DefaultTimestampScale, -10, FromMilli(-10)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FromScaledTimecode(tc.scale, tc.timecode); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
	if got := FromMilli(-5).Milliseconds(); got != -5 {
		t.Fatalf("negative millisecond round trip = %d", got)
	}
}

func TestRTPDeltaWraparound(t *testing.T) {
	cases := []struct {
		a, b uint32
		want int64
	}{
		{1000, 999, 1},
		{10, 10, 0},
		{0, 1, -1},
		{0xFFFFFFFE, 5, -7},     // backward distance across wrap
		{100, 0xFFFFFFFF, 101},  // forward wrap spans 101 ticks
		{0x80000000, 0, -2147483648},
	}
	for _, tc := range cases {
		if got := RTPDelta(tc.a, tc.b); got != tc.want {
			t.Fatalf("RTPDelta(%d,%d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestFromRTPTimestampWraparound(t *testing.T) {
	const rate uint32 = 48000
	base := FromNanoseconds(0)
	// One full 32-bit wrap is mathematically 2^32/48000 s; a step that
	// crosses the wrap boundary must still move forward monotonically.
	before := uint32(0xFFFFFFF0)
	after := uint32(16)
	tBefore := FromRTPTimestamp(rate, 0, base, before)
	tAfter := FromRTPTimestamp(rate, 0, base, after)
	delta := tAfter.Nanoseconds() - tBefore.Nanoseconds()
	want := RTPDelta(after, before) * NanosPerSecond / int64(rate)
	if delta != want {
		t.Fatalf("wrap delta = %d, want %d", delta, want)
	}
	if delta <= 0 {
		t.Fatalf("crossing wrap must advance time, got delta %d", delta)
	}
	// Zero clock rate anchors without panicking or inventing a rate.
	if got := FromRTPTimestamp(0, 5, base, 99); got != base {
		t.Fatalf("zero clock rate = %d, want anchor", got)
	}
}

func TestPipelineErrorClassificationAndChain(t *testing.T) {
	root := NewConflict("audio-normalize", "unmappable channel layout", "")
	next := BlockedBy("timeline-merge", root)
	leaf := BlockedBy("frame-export", next)

	if leaf.Class != ClassConflict {
		t.Fatalf("class must propagate, got %s", leaf.Class)
	}
	if leaf.Dependency != "timeline-merge" {
		t.Fatalf("immediate dependency = %q", leaf.Dependency)
	}
	if got := FirstBlocker(leaf); got != "audio-normalize" {
		t.Fatalf("FirstBlocker = %q, want audio-normalize", got)
	}
	if !errors.Is(leaf, root) {
		t.Fatal("errors.Is must find the root error through the chain")
	}
	if got := root.Class.String(); got != "conflict" {
		t.Fatalf("class string = %q", got)
	}

	boundary := NewBoundary("ebml-parse", "truncated block", 3, 4096, nil)
	if boundary.Class != ClassBoundary || boundary.Track != 3 || boundary.BlockOff != 4096 {
		t.Fatalf("boundary fields lost: %+v", boundary)
	}
	sentinel := errors.New("io error")
	internal := NewInternal("frame-export", "atomic rename failed", sentinel)
	if internal.Class != ClassInternal || !errors.Is(internal, sentinel) {
		t.Fatalf("internal error must wrap its cause: %+v", internal)
	}
}

func TestFirstBlockerOnNonPipelineError(t *testing.T) {
	if got := FirstBlocker(errors.New("plain")); got != "" {
		t.Fatalf("plain error must yield empty blocker, got %q", got)
	}
}
