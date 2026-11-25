package cut

import (
	"testing"
	"time"

	"github.com/pairwise/mediapack/internal/demux"
	"github.com/pairwise/mediapack/internal/timeline"
)

func TestCutsAtEveryKeyframeAndHashesDeterministically(t *testing.T) {
	frames := []demux.Frame{
		videoFrame(1, true, 0, []byte("k1")),
		videoFrame(2, false, 40*time.Millisecond, []byte("d1")),
		videoFrame(3, true, 80*time.Millisecond, []byte("k2-longer")),
		videoFrame(4, false, 120*time.Millisecond, []byte("d2")),
		{PTS: 10 * time.Millisecond, PTSTicks: 900, Codec: "pcm_s16le", Payload: []byte("audio-before-boundary"), PSIVersion: 1},
		{PTS: 90 * time.Millisecond, PTSTicks: 8100, Codec: "pcm_s16le", Payload: []byte("audio-in-second"), PSIVersion: 2},
	}
	clock := &timeline.Clock{}
	res, err := Build(frames, clock, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Segments) != 2 {
		t.Fatalf("segments=%d rejected=%d", len(res.Segments), len(res.Rejected))
	}
	for i, s := range res.Segments {
		if !s.Committable || !s.KeyFrame || len(s.Keyframes) != 1 {
			t.Fatalf("segment %d invalid: %+v", i, s)
		}
		if len(s.ClipSHA256) != 64 || len(s.ContentFP) != 64 {
			t.Fatalf("hash length bad: clip=%q fp=%q", s.ClipSHA256, s.ContentFP)
		}
		if s.Keyframes[0].BlobOffset != 0 {
			t.Fatalf("keyframe offset=%d, want 0", s.Keyframes[0].BlobOffset)
		}
	}
	if res.Segments[0].AudioFrames != 1 || res.Segments[1].AudioFrames != 1 {
		t.Fatalf("audio assignment=%d/%d", res.Segments[0].AudioFrames, res.Segments[1].AudioFrames)
	}

	res2, _ := Build(frames, &timeline.Clock{}, Config{})
	if res.Segments[0].ClipSHA256 != res2.Segments[0].ClipSHA256 {
		t.Fatal("same input produced different clip hash")
	}
}

func TestClipCrossingInvalidClockIsRejected(t *testing.T) {
	frames := []demux.Frame{
		videoFrame(1, true, 0, []byte("k1")),
		videoFrame(2, false, 40*time.Millisecond, []byte("d1")),
		videoFrame(3, true, 80*time.Millisecond, []byte("k2")),
	}
	clock := &timeline.Clock{}
	clock.MarkInvalid(30*time.Millisecond, 50*time.Millisecond, "unresolved PCR")
	res, err := Build(frames, clock, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Segments) != 1 || res.Segments[0].ID != "clip-0002" {
		t.Fatalf("only post-invalid keyframe clip should commit: %+v", res.Segments)
	}
	if len(res.Rejected) != 1 || res.Rejected[0].Committable {
		t.Fatalf("expected one rejected clip: %+v", res.Rejected)
	}
}

func videoFrame(seq uint32, key bool, pts time.Duration, payload []byte) demux.Frame {
	return demux.Frame{
		Sequence: seq, KeyFrame: key, PTS: pts,
		PTSTicks: uint64(pts) * timeline.PCRHz / uint64(time.Second),
		Codec:    "h264-synthetic", Payload: payload, PSIVersion: 1,
	}
}
