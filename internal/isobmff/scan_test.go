package isobmff

import (
	"path/filepath"
	"testing"

	"archivecheck/internal/clock"
	"archivecheck/internal/fault"
	"archivecheck/internal/fixture"
)

func genFile(t *testing.T, spec fixture.Spec) *Index {
	t.Helper()
	p := filepath.Join(t.TempDir(), "in.mp4")
	if err := fixture.Generate(p, spec); err != nil {
		t.Fatal(err)
	}
	f, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	return BuildIndexes(f, DefaultLimits)
}

func avSpec() fixture.Spec {
	return fixture.Spec{
		MovieTimescale: 1000,
		Tracks: []fixture.TrackSpec{
			{Handler: "vide", Timescale: 90000, SampleCount: 1000, SampleDelta: 3600,
				KeyframeEvery: 100, Width: 1920, Height: 1080, Color: &fixture.ColorBT709},
			{Handler: "soun", Timescale: 48000, SampleCount: 1875, SampleDelta: 1024,
				Channels: 2, SampleRate: 48000},
		},
	}
}

func firstFault(idx *Index, kind fault.Kind) *fault.Fault {
	for _, f := range idx.Faults {
		if f.Kind == kind {
			return f
		}
	}
	return nil
}

func TestCleanFileNoFaults(t *testing.T) {
	idx := genFile(t, avSpec())
	if len(idx.Faults) != 0 {
		t.Fatalf("unexpected faults: %v", idx.Faults)
	}
	if len(idx.Tracks) != 2 {
		t.Fatalf("tracks = %d", len(idx.Tracks))
	}
	v := idx.Tracks[0]
	if v.Declared != 1000 || v.Valid != 1000 {
		t.Fatalf("declared/valid = %d/%d", v.Declared, v.Valid)
	}
	if len(v.Keyframes) != 10 || v.Keyframes[0] != 1 || v.Keyframes[9] != 901 {
		t.Fatalf("keyframes = %v", v.Keyframes)
	}
	a := idx.Tracks[1]
	if a.Track.Channels != 2 || a.Track.SampleRate != 48000 {
		t.Fatalf("audio = %+v", a.Track)
	}
	if idx.File.Tracks[0].Color == nil || idx.File.Tracks[0].Color.Primaries != 1 {
		t.Fatalf("color = %+v", idx.File.Tracks[0].Color)
	}
}

func TestTruncatedSampleTableLocated(t *testing.T) {
	spec := avSpec()
	spec.Tracks[1].TruncateStszTo = 1449 // samples from 1450 on are lost
	idx := genFile(t, spec)
	fl := firstFault(idx, fault.KindTruncatedSampleTable)
	if fl == nil {
		t.Fatal("no truncation fault")
	}
	if fl.Track != 2 || fl.Sample != 1450 {
		t.Fatalf("fault at track=%d sample=%d, want 2/1450", fl.Track, fl.Sample)
	}
	if !fl.Recoverable || fl.FirstBlocked() != "normalize" {
		t.Fatalf("recoverable=%v chain=%v", fl.Recoverable, fl.Chain)
	}
	if idx.Tracks[1].Valid != 1449 {
		t.Fatalf("valid = %d, want 1449", idx.Tracks[1].Valid)
	}
}

func TestDuplicateTimestampLocated(t *testing.T) {
	spec := avSpec()
	spec.Tracks[0].ZeroDeltaAt = 100
	idx := genFile(t, spec)
	fl := firstFault(idx, fault.KindDuplicateTimestamp)
	if fl == nil {
		t.Fatal("no duplicate fault")
	}
	if fl.Track != 1 || fl.Sample != 100 {
		t.Fatalf("fault at track=%d sample=%d, want 1/100", fl.Track, fl.Sample)
	}
}

func TestEditListOutOfRangeLocated(t *testing.T) {
	spec := avSpec()
	// Media is 1000 samples (3.6M units at 90 kHz); the edit points at
	// 5.4M, past the end, so the first affected sample is one past the
	// last addressable sample (1001).
	spec.Tracks[0].Edits = []clock.Edit{{MediaTime: 5400000, Duration: 40000}}
	idx := genFile(t, spec)
	fl := firstFault(idx, fault.KindEditListOutOfRange)
	if fl == nil {
		t.Fatal("no edit-list fault")
	}
	if fl.Sample < 1000 || fl.Sample > 1002 {
		t.Fatalf("fault sample = %d, want within 1 of 1001", fl.Sample)
	}
}

func TestChunkOffsetOutOfRange(t *testing.T) {
	spec := avSpec()
	spec.Tracks[0].ChunkOffsetPlus = 1 << 20
	idx := genFile(t, spec)
	fl := firstFault(idx, fault.KindOffsetOutOfRange)
	if fl == nil {
		t.Fatal("no offset fault")
	}
	if fl.Track != 1 || fl.Sample != 1 {
		t.Fatalf("fault at track=%d sample=%d, want 1/1", fl.Track, fl.Sample)
	}
}

func TestOverlappingEditsRejected(t *testing.T) {
	spec := avSpec()
	spec.Tracks[0].Edits = []clock.Edit{
		{MediaTime: 0, Duration: 40000},
		{MediaTime: 1800000, Duration: 40000}, // inside edit 0's media range
	}
	idx := genFile(t, spec)
	if firstFault(idx, fault.KindEditListOverlap) == nil {
		t.Fatal("no overlap fault")
	}
}

func TestNonZeroStartAndEmptyEdit(t *testing.T) {
	spec := avSpec()
	spec.Tracks[0].Edits = []clock.Edit{
		{MediaTime: -1, Duration: 500, Empty: true},
		{MediaTime: 360000, Duration: 36000}, // start at media 4 s
	}
	idx := genFile(t, spec)
	if len(idx.Faults) != 0 {
		t.Fatalf("unexpected faults: %v", idx.Faults)
	}
	pts, err := idx.Tracks[0].KeyframePresentationTimes()
	if err != nil {
		t.Fatal(err)
	}
	// Keyframe 1 is at media 0, before the edit's media start: it maps to
	// 500ms (empty edit) + (0-360000)/90000 = -3.5s.
	if got := pts[0].Millis(); got != -3500 {
		t.Fatalf("first keyframe presentation = %d ms, want -3500", got)
	}
}

func TestLimitsEnforced(t *testing.T) {
	spec := avSpec()
	spec.Tracks[0].MediaDuration = 25 * 3600 * 90000 // 25 h > 24 h limit
	idx := genFile(t, spec)
	if firstFault(idx, fault.KindLimitExceeded) == nil {
		t.Fatal("no limit fault")
	}
}
