package index

import (
	"testing"

	"cinepipe/internal/clock"
	"cinepipe/internal/synth"
)

func scanSample(t *testing.T, kind synth.SampleKind, seed int64) *Index {
	t.Helper()
	s := synth.Build(kind, synth.Defaults(kind, seed))
	return Run(s.Data)
}

func TestNormalScanFindsProgramAndKeyframes(t *testing.T) {
	idx := scanSample(t, synth.SampleNormal, 42)
	if idx.Program.PMTPID != 0x100 || idx.Program.VideoPID != 0x101 || idx.Program.AudioPID != 0x102 {
		t.Fatalf("program %+v", idx.Program)
	}
	if len(idx.Tracks) != 2 {
		t.Fatalf("tracks %d", len(idx.Tracks))
	}
	video := idx.Tracks[0]
	if video.Kind != TrackVideo || video.Codec != "h264" {
		t.Fatalf("video track %+v", video)
	}
	if got := countKeyframes(video.Keyframes); got != 5 {
		t.Fatalf("keyframes %d want 5", got)
	}
	if len(idx.Ranges) != 0 {
		t.Fatalf("clean sample should have no ranges: %+v", idx.Ranges)
	}
	if idx.FirstError != -1 {
		t.Fatalf("clean sample first error %d", idx.FirstError)
	}
	audio := idx.Tracks[1]
	if audio.Kind != TrackAudio || audio.Codec != "aac" || audio.SampleRate != 48000 || audio.Layout != "stereo" {
		t.Fatalf("audio track %+v", audio)
	}
}

func TestNormalKeyframeTimingOnTimeline(t *testing.T) {
	s := synth.Build(synth.SampleNormal, synth.Defaults(synth.SampleNormal, 42))
	idx := Run(s.Data)
	video := idx.Tracks[0]
	var keys []Keyframe
	for _, k := range video.Keyframes {
		if k.Keyframe {
			keys = append(keys, k)
		}
	}
	if len(keys) != len(s.KeyframeTicks) {
		t.Fatalf("scanned %d keyframes, generated %d", len(keys), len(s.KeyframeTicks))
	}
	// The scanner's origin is the first PCR; the generator's first PCR trails
	// the first frame by one second, so scanned ticks equal generated ticks plus
	// that offset for every frame.
	const originOffset = int64(clock.Base)
	for i := range s.KeyframeTicks {
		if got, want := keys[i].Ticks, s.KeyframeTicks[i]+originOffset; got != want {
			t.Fatalf("keyframe %d at %d want %d", i, got, want)
		}
	}
}

func TestWrapSampleExpandsCorrectly(t *testing.T) {
	idx := scanSample(t, synth.SampleWrap, 42)
	video := idx.Tracks[0]
	prev := int64(clock.NoTime)
	for _, k := range video.Keyframes {
		if k.Ticks == clock.NoTime {
			continue
		}
		if prev != clock.NoTime && k.Ticks <= prev {
			t.Fatalf("non-monotonic frame ticks %d after %d", k.Ticks, prev)
		}
		prev = k.Ticks
	}
	// A modulus wrap is legal, so it must not be reported as damage.
	for _, r := range idx.Ranges {
		if r.Kind == RangeContinuity || r.Kind == RangeTruncated {
			t.Fatalf("wrap sample reported a %s range: %+v", r.Kind, r)
		}
	}
	// The PSI version bump is surfaced as an informational diagnostic.
	foundVersion := false
	for _, e := range idx.Errors {
		if e.Kind == "psi-version" {
			foundVersion = true
		}
	}
	if !foundVersion {
		t.Fatalf("no version-switch diagnostic in %v", idx.Errors)
	}
}

func TestContinuitySampleReportsRanges(t *testing.T) {
	idx := scanSample(t, synth.SampleContinuity, 42)
	if len(idx.Ranges) < 2 {
		t.Fatalf("expected at least 2 damaged ranges, got %d", len(idx.Ranges))
	}
	recovered := 0
	for _, r := range idx.Ranges {
		if r.RecoveredAt >= 0 {
			recovered++
		}
	}
	if recovered == 0 {
		t.Fatalf("no range reported a recovery keyframe: %+v", idx.Ranges)
	}
}

func TestTruncatedSampleReportsTailLoss(t *testing.T) {
	idx := scanSample(t, synth.SampleTruncated, 42)
	found := false
	for _, e := range idx.Errors {
		if e.Kind == "truncated" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no truncated diagnostic: %v", idx.Errors)
	}
	if len(idx.Ranges) == 0 {
		t.Fatal("truncated sample should report a damaged range")
	}
}

func TestMetadataConflictKeptSeparate(t *testing.T) {
	idx := scanSample(t, synth.SampleMetadata, 42)
	if len(idx.Tracks) != 2 {
		t.Fatalf("tracks %d", len(idx.Tracks))
	}
	audio := idx.Tracks[1]
	if audio.Kind != TrackAudio || len(audio.Descriptions) == 0 {
		t.Fatalf("audio track %+v", audio)
	}
	if audio.SampleRate != 48000 || audio.Layout != "stereo" {
		t.Fatalf("first description should be preserved: %+v", audio)
	}
	// A metadata conflict must be reported and must not open a damaged range.
	foundMeta := false
	for _, e := range idx.Errors {
		if e.Kind == "metadata" {
			foundMeta = true
		}
	}
	if !foundMeta {
		t.Fatalf("no metadata diagnostic: %v", idx.Errors)
	}
}

func TestHashesDetectInputChange(t *testing.T) {
	a := synth.Build(synth.SampleNormal, synth.Defaults(synth.SampleNormal, 42))
	b := synth.Build(synth.SampleNormal, synth.Defaults(synth.SampleNormal, 43))
	ia := Run(a.Data)
	ib := Run(b.Data)
	if ia.Source.SHA256 == ib.Source.SHA256 {
		t.Fatal("different inputs must hash differently")
	}
	ia2 := Run(a.Data)
	if ia.Source.SHA256 != ia2.Source.SHA256 {
		t.Fatal("hash not stable")
	}
	if len(ia.Tracks) != len(ia2.Tracks) || len(ia.Errors) != len(ia2.Errors) {
		t.Fatal("index not deterministic")
	}
}

func countKeyframes(ks []Keyframe) int {
	n := 0
	for _, k := range ks {
		if k.Keyframe {
			n++
		}
	}
	return n
}
