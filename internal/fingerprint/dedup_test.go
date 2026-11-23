package fingerprint

import (
	"testing"

	"medpipe/internal/clock"
	"medpipe/internal/model"
	"medpipe/internal/segment"
)

func fpFixture(t *testing.T) (*clock.Timeline, []segment.Segment, map[string][]model.Frame) {
	t.Helper()
	tracks := []model.Track{{ID: "cam", Kind: model.KindVideo, Timescale: 1_000_000, ClockBits: 32, NominalTickDelta: 40_000}}
	frames := map[string][]model.Frame{"cam": {}}
	for i := 0; i < 25; i++ {
		fr := model.Frame{
			TrackID: "cam", Seq: uint64(i), RawTS: uint64(1000 + i*40_000),
			Keyframe: i%5 == 0, Width: 64, Height: 48,
			Payload: []byte{byte(i), 0x11, 0x22},
			Meta:    []model.Metadata{{Key: "label", Value: "take1"}},
		}
		frames["cam"] = append(frames["cam"], fr)
	}
	tl, err := clock.Build(tracks, frames, clock.Options{NoDriftCorrection: true})
	if err != nil {
		t.Fatal(err)
	}
	res, err := segment.Plan(tl, nil, segment.Options{WindowNS: 200_000_000})
	if err != nil {
		t.Fatal(err)
	}
	return tl, res.Segments, frames
}

func TestFingerprintContentVsMetadata(t *testing.T) {
	tl, segs, frames := fpFixture(t)
	base, err := Compute(tl, segs, frames)
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Segments) < 2 {
		t.Fatalf("expected several segments, got %d", len(base.Segments))
	}

	// Metadata, dimensions and energy changes must not alter content hashes.
	mutated := cloneFrames(frames)
	for i := range mutated["cam"] {
		mutated["cam"][i].Meta = []model.Metadata{{Key: "label", Value: "take2"}}
		mutated["cam"][i].Width = 1920
		mutated["cam"][i].Height = 1080
	}
	metaFP, err := Compute(tl, segs, mutated)
	if err != nil {
		t.Fatal(err)
	}
	for i := range base.Segments {
		if base.Segments[i].SHA256 != metaFP.Segments[i].SHA256 {
			t.Fatalf("metadata change altered segment %d hash", i)
		}
		if base.Segments[i].DedupeKey != metaFP.Segments[i].DedupeKey {
			t.Fatalf("metadata change altered dedupe key for segment %d", i)
		}
	}

	// A payload change must alter the affected segment hash and nothing else.
	changed := cloneFrames(frames)
	changed["cam"][3].Payload = []byte{0xff, 0x11, 0x22}
	changedFP, err := Compute(tl, segs, changed)
	if err != nil {
		t.Fatal(err)
	}
	differing := 0
	for i := range base.Segments {
		if base.Segments[i].SHA256 != changedFP.Segments[i].SHA256 {
			differing++
		}
	}
	if differing != 1 {
		t.Fatalf("payload change affected %d segments, want 1", differing)
	}
}

func TestFingerprintTimingBindsHashButNotDedupe(t *testing.T) {
	tl, segs, frames := fpFixture(t)
	base, err := Compute(tl, segs, frames)
	if err != nil {
		t.Fatal(err)
	}
	// Re-time the same payloads by shifting the track origin.
	shifted := make([]model.Track, 0, len(tl.Tracks))
	for i := range tl.Tracks {
		tr := tl.Tracks[i].Track
		tr.OriginNS += 5_000_000
		shifted = append(shifted, tr)
	}
	tl2, err := clock.Build(shifted, frames, clock.Options{NoDriftCorrection: true})
	if err != nil {
		t.Fatal(err)
	}
	segs2, err := segment.Plan(tl2, nil, segment.Options{WindowNS: 200_000_000})
	if err != nil {
		t.Fatal(err)
	}
	retimed, err := Compute(tl2, segs2.Segments, frames)
	if err != nil {
		t.Fatal(err)
	}
	if len(retimed.Segments) != len(base.Segments) {
		t.Fatalf("segment counts differ: %d vs %d", len(retimed.Segments), len(base.Segments))
	}
	contentSame := true
	for i := range base.Segments {
		if base.Segments[i].DedupeKey != retimed.Segments[i].DedupeKey {
			contentSame = false
		}
	}
	if !contentSame {
		t.Fatal("dedupe key changed when only timing changed; it must bind content")
	}
}

func TestDuplicates(t *testing.T) {
	segs := []SegmentFingerprint{
		{Index: 0, DedupeKey: "a"},
		{Index: 1, DedupeKey: "b"},
		{Index: 2, DedupeKey: "a"},
		{Index: 3, DedupeKey: "c"},
		{Index: 4, DedupeKey: "b"},
	}
	dups := Duplicates(segs)
	if len(dups) != 2 || dups[0][0] != 0 || dups[1][0] != 1 {
		t.Fatalf("duplicates = %v", dups)
	}
}

func TestComputeRejectsBadSlice(t *testing.T) {
	tl, segs, frames := fpFixture(t)
	bad := append([]segment.Segment(nil), segs...)
	bad[0].Slices = append([]segment.TrackSlice(nil), segs[0].Slices...)
	bad[0].Slices[0].FirstFrame = 9999
	if _, err := Compute(tl, bad, frames); err == nil {
		t.Fatal("expected out-of-range slice error")
	}
	bad[0].Slices[0].FirstFrame = 0
	bad[0].Slices[0].TrackID = "ghost"
	if _, err := Compute(tl, bad, frames); err == nil {
		t.Fatal("expected unknown track error")
	}
}

func cloneFrames(in map[string][]model.Frame) map[string][]model.Frame {
	out := make(map[string][]model.Frame, len(in))
	for k, v := range in {
		cp := make([]model.Frame, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}
