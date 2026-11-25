package index_test

import (
	"testing"

	"archivecheck/internal/demux"
	"archivecheck/internal/index"
	"archivecheck/internal/media"
)

// avccSample builds an AVCC-framed H.264 sample; key=true uses an IDR NAL.
func avccSample(i int, key bool) demux.Sample {
	nalType := byte(1)
	if key {
		nalType = 5
	}
	nal := []byte{0x60 | nalType, byte(i), 0xAA, 0xBB}
	data := make([]byte, 4+len(nal))
	data[3] = byte(len(nal))
	copy(data[4:], nal)
	return demux.Sample{
		Track: 1, Index: i, Data: data, Offset: int64(100 + i*10),
		Duration: 1000, TimeBase: media.TimeBaseVideo90k, Codec: demux.CodecAVC1,
	}
}

func videoTrack(stss []int, n int) demux.TrackSamples {
	samples := make([]demux.Sample, n)
	for i := range samples {
		samples[i] = avccSample(i, i%3 == 0)
	}
	return demux.TrackSamples{
		Meta:    demux.TrackMeta{Track: 1, Handler: "vide", Keyframes: stss, TimeBase: media.TimeBaseVideo90k},
		Samples: samples,
	}
}

// TestTrustsHealthyIndex is the happy path: a correct stss is used directly.
func TestTrustsHealthyIndex(t *testing.T) {
	ts := videoTrack([]int{1, 4, 7, 10}, 12) // matches i%3==0 (0,3,6,9 -> 1-based)
	kf, rep := index.IndexFor(ts, index.ScanBudget)
	if rep.Rebuilt {
		t.Fatal("healthy stss must not trigger a rebuild")
	}
	if len(kf) != 4 {
		t.Fatalf("keyframes = %d, want 4", len(kf))
	}
}

// TestRebuildMissingIndex covers recovery when stss is absent: scan finds the
// IDR samples and records a located missing-index fault.
func TestRebuildMissingIndex(t *testing.T) {
	ts := videoTrack(nil, 12)
	kf, rep := index.IndexFor(ts, index.ScanBudget)
	if !rep.Rebuilt {
		t.Fatal("missing stss must force a rebuild")
	}
	if len(kf) != 4 {
		t.Fatalf("rebuilt keyframes = %d, want 4 (samples 0,3,6,9)", len(kf))
	}
	var sawMissing bool
	for _, f := range rep.Faults {
		if f.Kind == media.KindMissingIndex && f.Track == 1 {
			sawMissing = true
		}
	}
	if !sawMissing {
		t.Fatal("missing-index fault not recorded with track number")
	}
}

// TestRebuildCorruptIndex proves a lying stss (a P-frame claimed as sync) is
// rejected and replaced by the scan result rather than trusted.
func TestRebuildCorruptIndex(t *testing.T) {
	// Sample 2 (1-based) is a P-frame, but the corrupt stss lists it.
	ts := videoTrack([]int{2, 4, 7, 10}, 12)
	kf, rep := index.IndexFor(ts, index.ScanBudget)
	if !rep.Rebuilt {
		t.Fatal("corrupt stss must force a rebuild")
	}
	for _, k := range kf {
		if k.SampleIndex == 1 { // 0-based sample 1 = the falsely claimed P-frame
			t.Fatal("phantom keyframe from corrupt stss survived the rebuild")
		}
	}
	var sawCorrupt bool
	for _, f := range rep.Faults {
		if f.Kind == media.KindCorruptIndex {
			sawCorrupt = true
		}
	}
	if !sawCorrupt {
		t.Fatal("corrupt-index fault not recorded")
	}
}

// TestScanBudgetLimitsBytes ensures the scan stops at the byte cap and reports
// a located scan-limit fault instead of reading unbounded data.
func TestScanBudgetLimitsBytes(t *testing.T) {
	// Each sample is 8 coded bytes; a 17-byte budget permits only 2 samples
	// (the third would push scanned to 24 and trip the limit).
	ts := videoTrack(nil, 12)
	_, rep := index.IndexFor(ts, 17)
	if !rep.Rebuilt || !rep.ScanLimited {
		t.Fatalf("expected scan-limit, rebuilt=%v limited=%v", rep.Rebuilt, rep.ScanLimited)
	}
	if rep.Scanned > 17+8 { // at most one sample may cross the boundary check
		t.Fatalf("scanned %d bytes beyond the bounded expectation", rep.Scanned)
	}
	var sawLimit bool
	for _, f := range rep.Faults {
		if f.Kind == media.KindScanLimit && f.Offset >= 0 {
			sawLimit = true
		}
	}
	if !sawLimit {
		t.Fatal("scan-limit fault must carry an offset")
	}
}

// TestMalformedFramingNoPhantomKeyframe guarantees an unparseable sample is
// never promoted to a keyframe.
func TestMalformedFramingNoPhantomKeyframe(t *testing.T) {
	bad := demux.Sample{
		Track: 1, Index: 0, Data: []byte{0x00, 0x00, 0x10, 0x00, 0x65}, // length 4096, truncated
		Codec: demux.CodecAVC1,
	}
	ts := demux.TrackSamples{
		Meta:    demux.TrackMeta{Track: 1, Handler: "vide"},
		Samples: []demux.Sample{bad},
	}
	kf, _ := index.IndexFor(ts, index.ScanBudget)
	if len(kf) != 0 {
		t.Fatal("malformed sample must not yield a keyframe")
	}
}

// TestCutPointLookup verifies nearest-keyframe alignment for edit points.
func TestCutPointLookup(t *testing.T) {
	ts := videoTrack(nil, 12)
	kf, _ := index.IndexFor(ts, index.ScanBudget)
	k, ok := index.NearestKeyframeAtOrBefore(kf, 5)
	if !ok || k.SampleIndex != 3 {
		t.Fatalf("cut lookup = %+v ok=%v, want sample 3", k, ok)
	}
	if _, ok := index.NearestKeyframeAtOrBefore(kf, -1); ok {
		t.Fatal("no keyframe should exist before the first sample")
	}
}

// TestNonVideoTrackEmpty confirms audio/text tracks get no keyframe index.
func TestNonVideoTrackEmpty(t *testing.T) {
	ts := demux.TrackSamples{Meta: demux.TrackMeta{Track: 2, Handler: "soun"}}
	kf, rep := index.IndexFor(ts, index.ScanBudget)
	if len(kf) != 0 || rep.Rebuilt {
		t.Fatal("non-video track must have an empty, non-rebuilt index")
	}
}
