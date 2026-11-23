package segment

import (
	"testing"

	"mediad/diag"
	"mediad/timeline"
)

// makeFrames builds frames at a rational fps. Keyframes land every keyEvery
// frames, shifted by phase frames (so different representations can have
// slightly offset keyframe grids).
func makeFrames(fr timeline.FrameRate, count int, keyEvery, phase int) []Frame {
	out := make([]Frame, 0, count)
	for i := 0; i < count; i++ {
		tt, _ := timeline.FrameToTime(fr, int64(i))
		out = append(out, Frame{Time: tt, Keyframe: (i+phase)%keyEvery == 0})
	}
	return out
}

func TestAlignSharedCutPoints(t *testing.T) {
	// 30 fps, 90s => 2700 frames; keyframes every 90 frames (3s). All three
	// representations share the identical grid, so deviation must be zero.
	frames := makeFrames(timeline.FR30, 2700, 90, 0)
	reps := []Representation{
		{ID: "v-600", Bitrate: 600, Frames: frames},
		{ID: "v-1200", Bitrate: 1200, Frames: frames},
		{ID: "v-2400", Bitrate: 2400, Frames: frames},
	}
	m, err := Align(reps, Policy{TargetMS: 3000, MaxDeviationMS: 40})
	if err != nil {
		t.Fatal(err)
	}
	if m.SegmentCount() != 30 {
		t.Fatalf("segments = %d, want 30", m.SegmentCount())
	}
	if m.MaxAlignmentErrMS != 0 {
		t.Fatalf("aligned grids must have zero deviation, got %dms", m.MaxAlignmentErrMS)
	}
	// Every cut must be a keyframe for every representation.
	for _, s := range m.Segments {
		for _, id := range m.Representations {
			if !s.Cuts[id].Keyframe {
				t.Fatalf("seg %d rep %s cut is not a keyframe", s.Index, id)
			}
		}
	}
	// Segments tile the timeline without gaps or overlaps.
	for i := 1; i < len(m.Segments); i++ {
		if m.Segments[i].Start.Cmp(m.Segments[i-1].End) != 0 {
			t.Fatalf("gap/overlap between segment %d and %d", i-1, i)
		}
	}
}

func TestAlignKeyframeExtensionAndDeviation(t *testing.T) {
	// Anchor keyframes on the 3s grid; second rep shifted by one frame
	// (33.3ms), inside the 40ms tolerance. Boundaries extend to the nearest
	// keyframe and the deviation is recorded, never silently ignored.
	anchor := makeFrames(timeline.FR30, 600, 90, 0)
	shifted := makeFrames(timeline.FR30, 600, 90, 1) // frame 0 not a keyframe
	shifted[0].Keyframe = true
	reps := []Representation{
		{ID: "a", Frames: anchor},
		{ID: "b", Frames: shifted},
	}
	m, err := Align(reps, Policy{TargetMS: 3000, MaxDeviationMS: 40})
	if err != nil {
		t.Fatal(err)
	}
	if m.MaxAlignmentErrMS < 33 {
		t.Fatalf("expected ~33ms recorded deviation, got %dms", m.MaxAlignmentErrMS)
	}
	// Segment 0 starts at the shared origin (no deviation); later cuts extend
	// to the shifted grid and must carry the recorded ~33ms deviation.
	if m.Segments[1].Cuts["b"].DeviationMS < 33 {
		t.Fatalf("cut deviation not recorded: %+v", m.Segments[1].Cuts["b"])
	}
}

func TestAlignRejectsExcessiveDeviation(t *testing.T) {
	// Shifted grid is two frames (66ms) away; tolerance 40ms must fail loudly.
	anchor := makeFrames(timeline.FR30, 600, 90, 0)
	shifted := makeFrames(timeline.FR30, 600, 90, 2)
	shifted[0].Keyframe = true
	_, err := Align(
		[]Representation{{ID: "a", Frames: anchor}, {ID: "b", Frames: shifted}},
		Policy{TargetMS: 3000, MaxDeviationMS: 40},
	)
	if diag.CodeOf(err) != diag.ENoKeyframe {
		t.Fatalf("deviation beyond tolerance must be E_NO_KEYFRAME, got %v", err)
	}
}

func TestAlignNoKeyframesAtAll(t *testing.T) {
	fr := makeFrames(timeline.FR25, 100, 90, 0)
	for i := range fr {
		fr[i].Keyframe = false
	}
	_, err := Align([]Representation{{ID: "x", Frames: fr}}, Policy{TargetMS: 2000, MaxDeviationMS: 40})
	if diag.CodeOf(err) != diag.ENoKeyframe {
		t.Fatalf("keyless rep must be E_NO_KEYFRAME, got %v", err)
	}
}

func TestManifestDeterministicHash(t *testing.T) {
	frames := makeFrames(timeline.FR30, 900, 90, 0)
	reps := []Representation{{ID: "r1", Frames: frames}, {ID: "r2", Frames: frames}}
	m1, err := Align(reps, Policy{TargetMS: 3000, MaxDeviationMS: 40})
	if err != nil {
		t.Fatal(err)
	}
	// A second, independently built but identical manifest hashes identically.
	m2, err := Align(
		[]Representation{{ID: "r2", Frames: frames}, {ID: "r1", Frames: frames}},
		Policy{TargetMS: 3000, MaxDeviationMS: 40},
	)
	if err != nil {
		t.Fatal(err)
	}
	if m1.SHA256() != m2.SHA256() {
		t.Fatalf("manifest hash not input-order independent:\n%s\n%s", m1.Canonical(), m2.Canonical())
	}
	if len(m1.SHA256()) != 64 {
		t.Fatalf("sha256 hex must be 64 chars, got %d", len(m1.SHA256()))
	}
}

func TestAlignValidation(t *testing.T) {
	if _, err := Align(nil, Policy{TargetMS: 1000}); diag.CodeOf(err) != diag.EParam {
		t.Fatalf("empty reps must be E_PARAM, got %v", err)
	}
	frames := makeFrames(timeline.FR30, 10, 3, 0)
	if _, err := Align([]Representation{{ID: "x", Frames: frames}}, Policy{TargetMS: 0});
		diag.CodeOf(err) != diag.EParam {
		t.Fatalf("zero target must be E_PARAM, got %v", err)
	}
}
