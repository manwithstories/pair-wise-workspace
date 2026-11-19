package fingerprint_test

import (
	"testing"

	"mediasync/internal/fingerprint"
	"mediasync/internal/media"
	"mediasync/internal/mp4"
	"mediasync/internal/segment"
	"mediasync/internal/testdata"
	"mediasync/internal/timeline"
)

func track(t *testing.T, spec testdata.MP4Spec) (*mp4.Track, []byte) {
	t.Helper()
	data := testdata.GenerateMP4(spec)
	tracks := mp4.Index(data, "x.mp4")
	if len(tracks) != 1 || len(tracks[0].Samples) == 0 {
		t.Fatalf("fixture did not index: %+v", tracks)
	}
	return tracks[0], data
}

func timebase(t *testing.T, rate string, domain string) timeline.Timebase {
	t.Helper()
	r, err := timeline.ParseRate(rate)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := timeline.NewTimebase(r, timeline.Clock{Domain: domain, Ticks: 90000})
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

func wholeSegment(tr *mp4.Track) segment.Segment {
	return segment.Segment{Index: 0, StartNS: tr.Samples[0].DTSNS,
		EndNS: tr.Samples[len(tr.Samples)-1].EndNS(), StartSample: 0, EndSample: len(tr.Samples) - 1}
}

func TestIdenticalContentMatchesAcrossCameras(t *testing.T) {
	trA, fileA := track(t, testdata.DefaultSpec(42))
	// Camera B stores the same payloads after extra padding bytes, so every
	// absolute offset differs.
	specB := testdata.DefaultSpec(42)
	specB.PaddingBytes = 256
	trB, fileB := track(t, specB)
	if trA.Samples[0].Offset == trB.Samples[0].Offset {
		t.Fatal("fixtures share offsets; cross-camera test is not meaningful")
	}
	tb := timebase(t, "25", "cam-a")
	a := fingerprint.Build("cam-a", wholeSegment(trA), trA, fileA, tb, "same transcript", nil)
	b := fingerprint.Build("cam-b", wholeSegment(trB), trB, fileB, tb, "same transcript", nil)
	if a.Hash == "" || b.Hash == "" {
		t.Fatalf("incomplete hash: a=%q b=%q", a.Flags.IncompleteReason(), b.Flags.IncompleteReason())
	}
	if a.Hash != b.Hash {
		t.Fatalf("identical content hashed differently:\n a=%s\n b=%s", a.Hash, b.Hash)
	}
	if fingerprint.Short(a.Hash) != a.Hash[:16] {
		t.Errorf("Short() = %q", fingerprint.Short(a.Hash))
	}
	matches, issues := fingerprint.Compare([]fingerprint.Clip{a, b})
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	if len(matches) != 1 || len(matches[0].Cameras) != 2 {
		t.Fatalf("cross-camera duplicate not found: %+v", matches)
	}
	if matches[0].Cameras[0] != "cam-a" || matches[0].Cameras[1] != "cam-b" {
		t.Errorf("camera order = %v", matches[0].Cameras)
	}
}

func TestDifferentContentDoesNotMatch(t *testing.T) {
	trA, fileA := track(t, testdata.DefaultSpec(42))
	trB, fileB := track(t, testdata.DefaultSpec(43))
	tb := timebase(t, "25", "cam-a")
	a := fingerprint.Build("cam-a", wholeSegment(trA), trA, fileA, tb, "", nil)
	b := fingerprint.Build("cam-b", wholeSegment(trB), trB, fileB, tb, "", nil)
	if a.Hash == b.Hash {
		t.Fatal("different payloads produced the same hash")
	}
	matches, _ := fingerprint.Compare([]fingerprint.Clip{a, b})
	if len(matches) != 0 {
		t.Errorf("false-positive duplicate: %+v", matches)
	}
}

func TestHashIsDeterministic(t *testing.T) {
	tr, file := track(t, testdata.DefaultSpec(7))
	tb := timebase(t, "25", "cam-a")
	first := fingerprint.Build("cam", wholeSegment(tr), tr, file, tb, "text", []int64{0, 10})
	second := fingerprint.Build("cam", wholeSegment(tr), tr, file, tb, "text", []int64{0, 10})
	if first.Hash != second.Hash {
		t.Fatalf("hash not deterministic: %s vs %s", first.Hash, second.Hash)
	}
	if len(first.Hash) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars", len(first.Hash))
	}
}

func TestTranscriptAffectsHash(t *testing.T) {
	tr, file := track(t, testdata.DefaultSpec(42))
	tb := timebase(t, "25", "cam-a")
	a := fingerprint.Build("cam", wholeSegment(tr), tr, file, tb, "one", nil)
	b := fingerprint.Build("cam", wholeSegment(tr), tr, file, tb, "two", nil)
	if a.Hash == b.Hash {
		t.Error("different transcripts must change the hash")
	}
}

func TestMissingDataNeverHashes(t *testing.T) {
	tr, file := track(t, testdata.DefaultSpec(42))
	tb := timebase(t, "25", "cam-a")
	// Simulate samples pointing outside the file.
	broken := *tr
	broken.Samples = append([]mp4.Sample(nil), tr.Samples...)
	broken.Samples[5].Offset = int64(len(file)) + 10
	clip := fingerprint.Build("cam", wholeSegment(&broken), &broken, file, tb, "", nil)
	if clip.Hash != "" {
		t.Fatal("clip with out-of-file samples must not be hashed")
	}
	if !clip.Flags.Incomplete {
		t.Fatal("incomplete flag not set")
	}
	matches, issues := fingerprint.Compare([]fingerprint.Clip{clip})
	if len(matches) != 0 {
		t.Errorf("incomplete clip participated in matching: %+v", matches)
	}
	if len(issues) == 0 {
		t.Error("incomplete clip produced no issue")
	}
	if string(issues[0].Stage) != "fingerprint" {
		t.Errorf("issue stage = %v", issues[0].Stage)
	}
}

func TestIncompatibleTimebaseBlocksComparison(t *testing.T) {
	trA, fileA := track(t, testdata.DefaultSpec(42))
	trB, fileB := track(t, testdata.DefaultSpec(42))
	a := fingerprint.Build("cam-a", wholeSegment(trA), trA, fileA, timebase(t, "25", "a"), "", nil)
	broken := timebase(t, "25", "b")
	broken.Rate = timeline.Rate{}
	b := fingerprint.Build("cam-b", wholeSegment(trB), trB, fileB, broken, "", nil)
	if reason := a.Flags.Incompatible(b.Flags); reason == "" {
		t.Fatal("an unusable timebase must be reported as incompatible")
	}
	// Two cameras at different frame rates are still comparable: content
	// identity does not depend on the frame grid.
	cross := fingerprint.Build("cam-d", wholeSegment(trB), trB, fileB, timebase(t, "30000/1001", "d"), "", nil)
	if reason := a.Flags.Incompatible(cross.Flags); reason != "" {
		t.Errorf("cross-rate comparison blocked as %q", reason)
	}
	// Same conversion factors but a different clock source stays comparable.
	c := fingerprint.Build("cam-c", wholeSegment(trB), trB, fileB, timebase(t, "25", "elsewhere"), "", nil)
	if reason := a.Flags.Incompatible(c.Flags); reason != "" {
		t.Errorf("same conversion factors blocked as %q", reason)
	}
}

func TestDuplicateGrouping(t *testing.T) {
	tr, file := track(t, testdata.DefaultSpec(42))
	tb := timebase(t, "25", "cam")
	seg := wholeSegment(tr)
	var clips []fingerprint.Clip
	for _, cam := range []string{"cam-a", "cam-b", "cam-c"} {
		clips = append(clips, fingerprint.Build(cam, seg, tr, file, tb, "", nil))
	}
	matches, issues := fingerprint.Compare(clips)
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(matches))
	}
	if len(matches[0].Cameras) != 3 || len(matches[0].Segments) != 3 {
		t.Errorf("match = %+v", matches[0])
	}
}

func TestNilTimebaseBlocksComparison(t *testing.T) {
	var a, b fingerprint.Flags
	if a.Incompatible(b) == "" {
		t.Error("flags without a timebase must not be comparable")
	}
}

func TestCompareRejectsIncompatibleGroup(t *testing.T) {
	tr, file := track(t, testdata.DefaultSpec(42))
	seg := wholeSegment(tr)
	a := fingerprint.Build("cam-a", seg, tr, file, timebase(t, "25", "a"), "", nil)
	broken := timebase(t, "25", "b")
	broken.Rate = timeline.Rate{}
	b := fingerprint.Build("cam-b", seg, tr, file, broken, "", nil)
	if a.Hash == "" || b.Hash == "" {
		t.Fatal("test fixtures failed to hash")
	}
	matches, issues := fingerprint.Compare([]fingerprint.Clip{a, b})
	if len(matches) != 0 {
		t.Fatalf("incompatible clips were matched: %+v", matches)
	}
	if len(issues) != 1 || issues[0].Code != media.CodeIncompatible {
		t.Fatalf("expected one incompatible issue, got %v", issues)
	}
	if issues[0].Offset != 0 && issues[0].HasOffset {
		t.Errorf("fingerprint issue should not claim a byte offset: %v", issues[0])
	}
}
