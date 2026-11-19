package container

import (
	"crypto/sha256"
	"testing"

	"mediapipeline/internal/mediaerr"
)

func audioTrack() TrackConfig {
	return TrackConfig{
		TrackID: 1, Timescale: 48000, IsVideo: false, Delta: 480,
		Samples: [][]byte{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}},
	}
}

func videoTrack() TrackConfig {
	sizes := []int{5, 7, 6, 8}
	keyframes := []bool{true, false, false, true}
	idx, merr := IndexFromSamples(2, keyframes, sizes)
	if merr != nil {
		panic(merr)
	}
	samples := make([][]byte, len(sizes))
	for i, n := range sizes {
		samples[i] = make([]byte, n)
		for j := range samples[i] {
			samples[i][j] = byte(i*10 + j)
		}
	}
	return TrackConfig{
		TrackID: 2, Timescale: 90000, IsVideo: true, Delta: 3600,
		Samples: samples, Keyframes: idx,
	}
}

func TestWriteParseRoundTrip(t *testing.T) {
	data, merr := Write([]TrackConfig{audioTrack(), videoTrack()})
	if merr != nil {
		t.Fatal(merr)
	}
	if len(data) == 0 {
		t.Fatal("empty container written")
	}
	parsed, merr := Parse(data)
	if merr != nil {
		t.Fatal(merr)
	}
	if len(parsed.Tracks) != 2 {
		t.Fatalf("tracks=%d want 2", len(parsed.Tracks))
	}
	if parsed.Tracks[0].TrackID != 1 || parsed.Tracks[0].Timescale != 48000 {
		t.Fatalf("audio track header wrong: %+v", parsed.Tracks[0])
	}
	if parsed.Tracks[1].TrackID != 2 || len(parsed.Tracks[1].KeyframeNums) != 2 {
		t.Fatalf("video keyframes wrong: %+v", parsed.Tracks[1])
	}
	if got := parsed.Tracks[1].KeyframeNums; got[0] != 1 || got[1] != 4 {
		t.Fatalf("keyframe sample numbers wrong: %v", got)
	}
	// Audio must not carry an stss box.
	if len(parsed.Tracks[0].KeyframeNums) != 0 {
		t.Fatal("audio track must have no keyframe index")
	}
	if rate := RoundTripConsistency([]TrackConfig{audioTrack(), videoTrack()}, parsed); rate != 1.0 {
		t.Fatalf("round-trip consistency=%f want 1.0", rate)
	}
	hit, merr := KeyframeHitRate(videoTrack(), parsed.Tracks[1])
	if merr != nil || hit != 1.0 {
		t.Fatalf("keyframe hit rate=%f err=%v", hit, merr)
	}
}

func TestChunkOffsetsBackfilledAndIncreasing(t *testing.T) {
	data, _ := Write([]TrackConfig{audioTrack(), videoTrack()})
	parsed, merr := Parse(data)
	if merr != nil {
		t.Fatal(merr)
	}
	off0 := parsed.Tracks[0].ChunkOffsets
	off1 := parsed.Tracks[1].ChunkOffsets
	if len(off0) != 1 || len(off1) != 1 {
		t.Fatalf("each track needs one chunk offset: %v %v", off0, off1)
	}
	if off0[0] == 0 {
		t.Fatal("stco placeholder was not backfilled")
	}
	if off1[0] <= off0[0] {
		t.Fatalf("video chunk offset %d must follow audio %d", off1[0], off0[0])
	}
}

func TestMdatPayloadHashStable(t *testing.T) {
	tracks := []TrackConfig{audioTrack(), videoTrack()}
	d1, _ := Write(tracks)
	d2, _ := Write(tracks)
	if sha256.Sum256(d1) != sha256.Sum256(d2) {
		t.Fatal("container output not deterministic for identical input")
	}
	p1, _ := Parse(d1)
	if len(p1.MdatPayload) == 0 {
		t.Fatal("mdat payload missing")
	}
}

func TestTruncatedContainerReported(t *testing.T) {
	data, _ := Write([]TrackConfig{audioTrack()})
	// Cut the file mid-container so a declared box runs past EOF.
	truncated := data[:len(data)*3/4]
	if _, merr := Parse(truncated); merr == nil ||
		(merr.Kind != mediaerr.KindTruncatedBox && merr.Kind != mediaerr.KindInvalidInput) {
		t.Fatalf("truncation must be reported, got %v", merr)
	}
	// Input shorter than any box header.
	if _, merr := Parse([]byte{0, 1, 2}); merr == nil ||
		merr.Kind != mediaerr.KindTruncatedBox {
		t.Fatalf("tiny input must report truncation, got %v", merr)
	}
}

func TestNoKeyframeVideoOmitsStss(t *testing.T) {
	idx, merr := IndexFromSamples(3, []bool{false, false, false}, []int{4, 4, 4})
	if merr == nil || merr.Kind != mediaerr.KindNoKeyframe {
		t.Fatalf("index build must flag no keyframe, got %v", merr)
	}
	tc := TrackConfig{
		TrackID: 3, Timescale: 90000, IsVideo: true, Delta: 3600,
		Samples: [][]byte{{1}, {2}, {3}}, Keyframes: idx,
	}
	data, merr := Write([]TrackConfig{tc})
	if merr != nil {
		t.Fatal(merr)
	}
	parsed, merr := Parse(data)
	if merr != nil {
		t.Fatal(merr)
	}
	if len(parsed.Tracks[0].KeyframeNums) != 0 {
		t.Fatal("no-keyframe track must omit stss, not fabricate hits")
	}
	if _, merr := KeyframeHitRate(tc, parsed.Tracks[0]); merr == nil ||
		merr.Kind != mediaerr.KindNoKeyframe {
		t.Fatalf("hit rate must surface no-keyframe, got %v", merr)
	}
}

func TestEmptyTrackRejected(t *testing.T) {
	if _, merr := Write(nil); merr == nil || merr.Kind != mediaerr.KindInvalidInput {
		t.Fatalf("empty track set must error, got %v", merr)
	}
}
