package manifest

import (
	"encoding/hex"
	"strings"
	"testing"

	"mediapack/internal/clock"
)

func twoReps() []Representation {
	return []Representation{
		{ID: "video-high", Tracks: []uint64{1}, Bitratebps: 2_000_000},
		{ID: "video-low", Tracks: []uint64{2}, Bitratebps: 500_000},
	}
}

func TestAlignSharedBoundariesAndCells(t *testing.T) {
	events := []InputEvent{
		{Track: 1, TimeNS: 100_000_000, PayloadLen: 100, Keyframe: true},
		{Track: 2, TimeNS: 150_000_000, PayloadLen: 40},
		{Track: 1, TimeNS: 600_000_000, PayloadLen: 120, Keyframe: true},
		{Track: 2, TimeNS: 700_000_000, PayloadLen: 45},
	}
	spec := GridSpec{GridNS: 500_000_000, ToleranceNS: 20_000_000, Reps: twoReps()}
	m, err := Align(events, spec, "fp")
	if err != nil {
		t.Fatal(err)
	}
	// Two cells (0-500, 500-1000) x two reps = 4 segments.
	if len(m.Segments) != 4 {
		t.Fatalf("want 4 segments, got %d", len(m.Segments))
	}
	// Shared boundaries: both reps at a given cell share StartNS/EndNS.
	if m.Segments[0].StartNS != m.Segments[1].StartNS ||
		m.Segments[0].EndNS != m.Segments[1].EndNS {
		t.Fatal("representations do not share time boundaries")
	}
	if m.Segments[0].Representation != "video-high" ||
		m.Segments[1].Representation != "video-low" {
		t.Fatalf("rep ordering wrong: %s, %s",
			m.Segments[0].Representation, m.Segments[1].Representation)
	}
	// First cell: video-high has 1 block/100 bytes, low has 1/40.
	if m.Segments[0].Blocks != 1 || m.Segments[0].Bytes != 100 {
		t.Fatalf("high cell0 wrong: %+v", m.Segments[0])
	}
	if m.Segments[1].Blocks != 1 || m.Segments[1].Bytes != 40 {
		t.Fatalf("low cell0 wrong: %+v", m.Segments[1])
	}
}

func TestChecksumDeterministic(t *testing.T) {
	events := []InputEvent{
		{Track: 1, TimeNS: 0, PayloadLen: 10, Keyframe: true},
		{Track: 2, TimeNS: 250_000_000, PayloadLen: 5},
	}
	spec := GridSpec{GridNS: 500_000_000, ToleranceNS: 10_000_000, Reps: twoReps()}
	m1, _ := Align(events, spec, "fp")
	m2, _ := Align(events, spec, "fp")
	c1, err := Checksum(m1)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := Checksum(m2)
	if c1 != c2 {
		t.Fatal("repeated alignment produced different checksums")
	}
	if len(c1) != 64 || c1 != strings.ToLower(c1) {
		t.Fatalf("checksum must be 64 lowercase hex chars, got %q", c1)
	}
	if _, err := hex.DecodeString(c1); err != nil {
		t.Fatalf("checksum not valid hex: %v", err)
	}
	b1, _ := CanonicalBytes(m1)
	b2, _ := CanonicalBytes(m2)
	if string(b1) != string(b2) {
		t.Fatal("canonical encodings differ")
	}
}

func TestMissingTrackAndEmptySegment(t *testing.T) {
	// Only track 1 has events; rep referencing track 9 must error.
	reps := []Representation{
		{ID: "good", Tracks: []uint64{1}},
		{ID: "broken", Tracks: []uint64{9}},
	}
	events := []InputEvent{{Track: 1, TimeNS: 0, PayloadLen: 7, Keyframe: true}}
	spec := GridSpec{GridNS: 500_000_000, ToleranceNS: 5_000_000, Reps: reps}
	m, err := Align(events, spec, "fp")
	if err != nil {
		t.Fatal(err)
	}
	var sawMissing, sawEmpty bool
	for _, se := range m.Errors {
		if se.Kind == "missing_track" && se.Track == 9 && se.Rep == "broken" {
			sawMissing = true
		}
		if se.Kind == "empty_segment" && se.Rep == "broken" {
			sawEmpty = true
		}
	}
	if !sawMissing {
		t.Error("missing_track structured error not recorded")
	}
	if !sawEmpty {
		t.Error("empty_segment structured error not recorded for broken rep")
	}
}

func TestUnmatchedBoundaryBeyondTolerance(t *testing.T) {
	// Keyframe 100ms into a 500ms cell with only 10ms tolerance: the
	// boundary cannot be matched onto the common grid.
	events := []InputEvent{
		{Track: 1, TimeNS: 100_000_000, PayloadLen: 9, Keyframe: true},
		{Track: 2, TimeNS: 100_000_000, PayloadLen: 3, Keyframe: true},
	}
	spec := GridSpec{GridNS: 500_000_000, ToleranceNS: 10_000_000, Reps: twoReps()}
	m, err := Align(events, spec, "fp")
	if err != nil {
		t.Fatal(err)
	}
	var unmatched int
	for _, se := range m.Errors {
		if se.Kind == "unmatched_boundary" {
			unmatched++
		}
	}
	if unmatched != 2 {
		t.Fatalf("want 2 unmatched boundary errors, got %d", unmatched)
	}
}

func TestAlignmentRejectsBadConfig(t *testing.T) {
	events := []InputEvent{{Track: 1, TimeNS: 0}}
	if _, err := Align(events, GridSpec{GridNS: 0, Reps: twoReps()}, "fp"); err == nil {
		t.Error("zero grid must error")
	}
	one := []Representation{{ID: "solo", Tracks: []uint64{1}}}
	if _, err := Align(events, GridSpec{GridNS: 10, Reps: one}, "fp"); err == nil {
		t.Error("single representation must error")
	} else if me, ok := clock.AsMediaError(err); !ok || me.Category != clock.CatAlignment {
		t.Fatalf("want CatAlignment, got %v", err)
	}
}

func TestFingerprintStable(t *testing.T) {
	data := []byte("deterministic source bytes")
	a := FingerprintBytes(data)
	b := FingerprintBytes(append([]byte(nil), data...))
	if a != b || len(a) != 64 {
		t.Fatal("input fingerprint must be stable and 64 hex chars")
	}
	if FingerprintBytes([]byte("other")) == a {
		t.Fatal("different inputs must hash differently")
	}
}

func TestEmptyInputManifest(t *testing.T) {
	m, err := Align(nil, GridSpec{GridNS: 100, ToleranceNS: 5, Reps: twoReps()}, "fp")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Segments) != 2 || !m.Segments[0].Empty || !m.Segments[1].Empty {
		t.Fatalf("empty input should yield empty cells: %+v", m.Segments)
	}
}
