package stream

import (
	"bytes"
	"testing"

	"github.com/abema/go-mp4"

	"mediascan/internal/fixture"
	"mediascan/internal/media"
)

func videoSpec(n int) fixture.TrackSpec {
	sizes := make([]uint32, n)
	for i := range sizes {
		sizes[i] = 100
	}
	return fixture.TrackSpec{
		Kind: "video", Timescale: 30000, SampleDelta: 1001,
		SampleSizes: sizes, SyncSamples: []int{1, 11, 21}, CorruptSizeIdx: -1,
	}
}

func TestAnalyzeMP4Basics(t *testing.T) {
	data := buildMP4(t, []fixture.TrackSpec{videoSpec(30)})
	fa, err := AnalyzeMP4(data, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fa.Tracks) != 1 {
		t.Fatalf("tracks = %d", len(fa.Tracks))
	}
	tr := fa.Tracks[0]
	if tr.Kind != "video" {
		t.Errorf("kind = %q", tr.Kind)
	}
	if len(tr.Samples) != 30 {
		t.Fatalf("samples = %d", len(tr.Samples))
	}
	// 29.97: 30 frames, each ~33.37ms.
	if tr.Samples[0].DTSNs != 0 {
		t.Errorf("sample0 dts = %d", tr.Samples[0].DTSNs)
	}
	if got := tr.Samples[1].DTSNs; got != 33_366_666 {
		t.Errorf("sample1 dts = %d, want 33366666", got)
	}
	// Sample byte spans must be contiguous and inside mdat.
	if tr.Samples[1].Offset != tr.Samples[0].Offset+100 {
		t.Errorf("sample1 offset = %d, sample0 offset+100 = %d", tr.Samples[1].Offset, tr.Samples[0].Offset+100)
	}
	if !tr.Complete {
		t.Errorf("expected complete index; issues: %v", fa.Issues)
	}
	if len(tr.Keyframes) != 3 {
		t.Errorf("keyframes = %v, want 3", tr.Keyframes)
	}
	if len(fa.CorruptRanges) != 0 {
		t.Errorf("unexpected corrupt ranges: %v", fa.CorruptRanges)
	}
}

func TestAnalyzeMP4Truncated(t *testing.T) {
	data := buildMP4(t, []fixture.TrackSpec{videoSpec(20)})
	// Truncate inside mdat (payload starts after moov; mdat header is 8 bytes).
	mdat := bytes.Index(data, []byte("mdat"))
	if mdat < 0 {
		t.Fatal("fixture has no mdat")
	}
	// Cut 500 bytes into the mdat payload: moov stays intact, later samples
	// fall past EOF.
	cut := data[:mdat+8+500]
	fa, err := AnalyzeMP4(cut, Limits{})
	if err != nil {
		t.Fatalf("truncation should be a recoverable issue, got error: %v", err)
	}
	// Track structure is still readable; samples past the cut are corrupt.
	if len(fa.Tracks) != 1 {
		t.Fatalf("tracks = %d", len(fa.Tracks))
	}
	if !hasClass(fa.Issues, media.ClassMP4CorruptSample) && !hasClass(fa.Issues, media.ClassMP4Truncated) {
		t.Errorf("truncation not reported: %v", fa.Issues)
	}
	if len(fa.CorruptRanges) == 0 {
		t.Errorf("truncation produced no corrupt range")
	}
	// The intact moov must not be reported as fatal.
	if fa.Issues.Fatal() {
		t.Errorf("truncated file reported as fatal: %v", fa.Issues)
	}
	// A recoverable subset of samples (those whose bytes remain) is kept.
	if len(fa.Tracks[0].Samples) == 0 {
		t.Error("no samples survived truncation")
	}
}

func TestAnalyzeMP4CorruptSampleSize(t *testing.T) {
	spec := videoSpec(15)
	spec.CorruptSizeIdx = 7
	data := buildMP4(t, []fixture.TrackSpec{spec})
	fa, err := AnalyzeMP4(data, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, is := range fa.Issues {
		if is.Class == media.ClassMP4CorruptSample && is.Locator.TrackID == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("corrupt sample size not located: %v", fa.Issues)
	}
	if len(fa.CorruptRanges) == 0 {
		t.Fatal("no corrupt range for corrupt sample")
	}
	r := fa.CorruptRanges[0]
	if r.TrackID != 1 || r.ByteStart <= 0 {
		t.Errorf("corrupt range lacks locator: %+v", r)
	}
}

func TestAnalyzeMP4MissingChunkIndex(t *testing.T) {
	spec := videoSpec(10)
	spec.OmitSTCO = true
	data := buildMP4(t, []fixture.TrackSpec{spec})
	fa, err := AnalyzeMP4(data, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasClass(fa.Issues, media.ClassMP4MissingIndex) {
		t.Fatalf("missing stco not reported: %v", fa.Issues)
	}
	if fa.Tracks[0].Complete {
		t.Error("track with no chunk offsets marked complete")
	}
}

func TestAnalyzeMP4NoKeyframes(t *testing.T) {
	spec := videoSpec(10)
	spec.SyncSamples = []int{}
	spec.NoSTSS = false
	// Empty stss => explicitly no sync samples.
	data := buildMP4(t, []fixture.TrackSpec{spec})
	fa, err := AnalyzeMP4(data, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasClass(fa.Issues, media.ClassMP4NoKeyframes) {
		t.Fatalf("no-keyframes not reported: %v", fa.Issues)
	}
}

func TestAnalyzeMP4ImplicitAllSync(t *testing.T) {
	spec := videoSpec(10)
	spec.NoSTSS = true
	data := buildMP4(t, []fixture.TrackSpec{spec})
	fa, err := AnalyzeMP4(data, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	tr := fa.Tracks[0]
	if tr.IndexSource != "implicit-all-sync" {
		t.Errorf("index source = %q", tr.IndexSource)
	}
	if len(tr.Keyframes) != 10 {
		t.Errorf("keyframes = %d, want 10", len(tr.Keyframes))
	}
}

func TestAnalyzeMP4SampleScanRebuildsIncompleteSTSS(t *testing.T) {
	// Payload markers cover every keyframe; stss declares only a subset. The
	// sample scan must detect the gap, restore the entries and mark the index
	// as rebuilt rather than silently trusting stss.
	spec := videoSpec(30)
	spec.KeyframeMarkers = []int{1, 11, 21}
	spec.SyncSamples = []int{1, 21} // 11 lost from stss
	data := buildMP4(t, []fixture.TrackSpec{spec})
	fa, err := AnalyzeMP4(data, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	tr := fa.Tracks[0]
	if tr.IndexSource != "rebuilt" {
		t.Fatalf("index source = %q, want rebuilt", tr.IndexSource)
	}
	if !hasClass(fa.Issues, media.ClassMP4MissingIndex) {
		t.Errorf("incomplete stss not reported: %v", fa.Issues)
	}
	want := map[int]bool{1: true, 11: true, 21: true}
	if len(tr.Keyframes) != 3 {
		t.Fatalf("keyframes = %v, want three", tr.Keyframes)
	}
	for _, k := range tr.Keyframes {
		if !want[k] {
			t.Errorf("unexpected rebuilt keyframe %d", k)
		}
	}
}

func TestAnalyzeMP4TwoTracksIsolated(t *testing.T) {
	video := videoSpec(12)
	video.CorruptSizeIdx = 5
	audio := fixture.TrackSpec{
		Kind: "audio", Timescale: 48000, SampleDelta: 1024,
		SampleSizes: []uint32{64, 64, 64, 64}, NoSTSS: true, CorruptSizeIdx: -1,
	}
	data := buildMP4(t, []fixture.TrackSpec{video, audio})
	fa, err := AnalyzeMP4(data, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fa.Tracks) != 2 {
		t.Fatalf("tracks = %d", len(fa.Tracks))
	}
	// Track 2 (audio) is untouched by track 1's corrupt sample.
	t1, t2 := fa.Tracks[0], fa.Tracks[1]
	if t1.TrackID != 1 || t2.TrackID != 2 {
		t.Fatalf("track ids = %d,%d", t1.TrackID, t2.TrackID)
	}
	if len(t2.Samples) != 4 {
		t.Errorf("audio samples = %d", len(t2.Samples))
	}
	for _, r := range fa.CorruptRanges {
		if r.TrackID != 1 {
			t.Errorf("corruption leaked to track %d", r.TrackID)
		}
	}
}

func TestAnalyzeMP4EmptyAndOversize(t *testing.T) {
	if _, err := AnalyzeMP4(nil, Limits{}); err == nil {
		t.Error("empty input accepted")
	}
	if _, err := AnalyzeMP4(make([]byte, 100), Limits{MaxInputBytes: 10}); err == nil {
		t.Error("oversized input accepted")
	}
}

func TestSampleOffsetsAccumulateWithinChunk(t *testing.T) {
	sizes := []uint32{10, 20, 30, 40}
	stsc := []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: 2}}
	chunks := []uint64{1000, 2000}
	got := sampleOffsets(sizes, stsc, chunks)
	want := []int64{1000, 1010, 2000, 2030}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("offsets = %v, want %v", got, want)
		}
	}
}

func hasClass(set media.IssueSet, c media.Class) bool {
	for _, is := range set {
		if is.Class == c {
			return true
		}
	}
	return false
}
