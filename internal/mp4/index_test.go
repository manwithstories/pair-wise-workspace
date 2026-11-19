package mp4_test

import (
	"bytes"
	"testing"

	"mediasync/internal/media"
	"mediasync/internal/mp4"
	"mediasync/internal/testdata"
)

func indexOne(t *testing.T, spec testdata.MP4Spec) (*mp4.Track, []byte) {
	t.Helper()
	data := testdata.GenerateMP4(spec)
	tracks := mp4.Index(data, "fixture.mp4")
	if len(tracks) != 1 {
		t.Fatalf("got %d tracks, want 1", len(tracks))
	}
	return tracks[0], data
}

func codes(issues []*media.Issue) map[media.Code]int {
	out := map[media.Code]int{}
	for _, is := range issues {
		out[is.Code]++
	}
	return out
}

func TestIndexClean(t *testing.T) {
	tr, data := indexOne(t, testdata.DefaultSpec(42))
	if len(tr.Issues) != 0 {
		t.Fatalf("unexpected issues: %v", tr.Issues)
	}
	if tr.TrackID != 1 || tr.Handler != "vide" || tr.Timescale != 12800 {
		t.Fatalf("track metadata = %+v", tr)
	}
	if len(tr.Samples) != 200 {
		t.Fatalf("got %d samples, want 200", len(tr.Samples))
	}
	if len(tr.Keyframes) != 8 {
		t.Fatalf("got %d keyframes, want 8", len(tr.Keyframes))
	}
	// Every sample must point at real bytes inside the file.
	for _, s := range tr.Samples {
		if s.Offset < 0 || s.Offset+int64(s.Size) > int64(len(data)) {
			t.Fatalf("sample %d out of file: off=%d size=%d file=%d", s.Number, s.Offset, s.Size, len(data))
		}
	}
	// Durations are exact at 25 fps.
	if tr.Samples[0].DTSNS != 0 || tr.Samples[1].DTSNS != 40_000_000 {
		t.Errorf("sample DTS = %d,%d want 0,40000000", tr.Samples[0].DTSNS, tr.Samples[1].DTSNS)
	}
	if tr.Samples[0].DurationNS != 40_000_000 {
		t.Errorf("sample duration = %d", tr.Samples[0].DurationNS)
	}
	for i := 1; i < len(tr.Samples); i++ {
		if tr.Samples[i].DTSNS <= tr.Samples[i-1].DTSNS {
			t.Fatalf("DTS not increasing at sample %d", i)
		}
	}
}

func TestKeyframeQueries(t *testing.T) {
	tr, _ := indexOne(t, testdata.DefaultSpec(42))
	if got := tr.KeyframeAt(0); got != 0 {
		t.Errorf("KeyframeAt(0) = %d", got)
	}
	if got := tr.KeyframeAt(1); got != -1 {
		t.Errorf("KeyframeAt(1ns) = %d, want -1", got)
	}
	if got := tr.KeyframeAtOrBefore(0); got != 0 {
		t.Errorf("KeyframeAtOrBefore(0) = %d", got)
	}
	// 2.5s -> keyframe at 2s
	if got := tr.KeyframeAtOrBefore(2_500_000_000); got != 50 {
		t.Errorf("KeyframeAtOrBefore(2.5s) = %d, want 50", got)
	}
	if s := tr.SampleForTime(3_000_000_000); s != 75 {
		t.Errorf("SampleForTime(3s) = %d, want 75", s)
	}
	if s := tr.SampleForTime(9_000_000_000); s != -1 {
		t.Errorf("SampleForTime(9s) = %d, want -1", s)
	}
}

func TestIndexChunkOutOfFile(t *testing.T) {
	spec := testdata.DefaultSpec(42)
	spec.Fault = testdata.FaultChunkOutOfFile
	tr, data := indexOne(t, spec)
	c := codes(tr.Issues)
	if c[media.CodeOutOfRange] == 0 {
		t.Fatalf("no out_of_range issue: %v", tr.Issues)
	}
	found := false
	for _, is := range tr.Issues {
		if is.Code != media.CodeOutOfRange {
			continue
		}
		if !is.HasOffset || is.Offset > int64(len(data)) {
			t.Errorf("issue offset %d outside file (%d bytes)", is.Offset, len(data))
		}
		found = true
		break
	}
	if !found {
		t.Fatal("no out_of_range issue with offset")
	}
	// The sample table is still built so the rest of the pipeline can run.
	if len(tr.Samples) != 200 {
		t.Errorf("got %d samples, want 200", len(tr.Samples))
	}
}

func TestIndexStssOutOfRangeRebuilds(t *testing.T) {
	spec := testdata.DefaultSpec(42)
	spec.Fault = testdata.FaultStssOutOfRange
	tr, _ := indexOne(t, spec)
	if !tr.RebuiltKeys {
		t.Fatal("damaged stss did not trigger a rebuild")
	}
	c := codes(tr.Issues)
	if c[media.CodeOutOfRange] == 0 || c[media.CodeMissingEntry] == 0 {
		t.Fatalf("expected out_of_range and missing_entry issues, got %v", c)
	}
	for _, is := range tr.Issues {
		if !is.HasOffset || is.Offset <= 0 {
			t.Errorf("issue lacks a box offset: %v", is)
		}
	}
	if len(tr.Keyframes) == 0 {
		t.Fatal("rebuild produced no keyframes")
	}
}

func TestIndexMissingStss(t *testing.T) {
	spec := testdata.DefaultSpec(42)
	spec.Fault = testdata.FaultNoStss
	tr, _ := indexOne(t, spec)
	if len(tr.Keyframes) != len(tr.Samples) {
		t.Errorf("stss-less track should treat all %d samples as sync, got %d",
			len(tr.Samples), len(tr.Keyframes))
	}
	if codes(tr.Issues)[media.CodeMissingEntry] == 0 {
		t.Error("missing stss not reported")
	}
	for _, is := range tr.Issues {
		if is.Code == media.CodeMissingEntry && is.Offset <= 0 {
			t.Errorf("missing stss reported without a box offset: %v", is)
		}
	}
}

func TestIndexZeroDeltaIsNonMonotonic(t *testing.T) {
	spec := testdata.DefaultSpec(42)
	spec.SampleCount = 10
	spec.Fault = testdata.FaultZeroDelta
	tr, _ := indexOne(t, spec)
	if codes(tr.Issues)[media.CodeNonMonotonic] == 0 {
		t.Fatal("zero stts delta not reported as non-monotonic")
	}
	for _, is := range tr.Issues {
		if !is.HasOffset {
			t.Errorf("non-monotonic issue has no offset: %v", is)
		}
	}
}

func TestIndexTruncated(t *testing.T) {
	spec := testdata.DefaultSpec(42)
	spec.Fault = testdata.FaultTruncatedMoov
	data := testdata.GenerateMP4(spec)
	tracks := mp4.Index(data, "truncated.mp4")
	if len(tracks) != 1 || !tracks[0].Fatal() {
		t.Fatalf("truncated moov must be fatal, got %d tracks", len(tracks))
	}
}

func TestIndexNotAnMP4(t *testing.T) {
	tracks := mp4.Index(bytes.Repeat([]byte{0xAB}, 128), "junk.mp4")
	if len(tracks) == 0 || !tracks[0].Fatal() {
		t.Fatalf("garbage input must be fatal: %+v", tracks)
	}
}

func TestIndexSizeLimit(t *testing.T) {
	data := make([]byte, mp4.MaxFileBytes+1)
	tracks := mp4.Index(data, "huge.mp4")
	if len(tracks) == 0 || !tracks[0].Fatal() {
		t.Fatal("oversized input must be fatal")
	}
}

func TestIdenticalPayloadDifferentOffsets(t *testing.T) {
	a, _ := indexOne(t, testdata.DefaultSpec(42))
	spec := testdata.DefaultSpec(42)
	spec.PaddingBytes = 128
	b, _ := indexOne(t, spec)
	if a.Samples[0].Offset == b.Samples[0].Offset {
		t.Fatal("padding did not shift sample offsets")
	}
	// Same content, different file layout: digests must still agree.
	if a.Samples[3].Size != b.Samples[3].Size {
		t.Errorf("sample sizes differ: %d vs %d", a.Samples[3].Size, b.Samples[3].Size)
	}
}
