package codec

import (
	"testing"

	"mediamix/internal/core"
	"mediamix/internal/timeline"
)

// h264Stream builds an Annex-B stream with an IDR frame then a non-IDR frame.
func h264Stream() ([]byte, []Sample) {
	idr := append([]byte{0, 0, 0, 1, 0x65}, make([]byte, 8)...)    // type 5
	nonIDR := append([]byte{0, 0, 0, 1, 0x41}, make([]byte, 8)...) // type 1
	stream := append(append([]byte{}, idr...), nonIDR...)
	samples := []Sample{
		{Offset: 0, Size: len(idr), PTS: 0, Key: true},
		{Offset: int64(len(idr)), Size: len(nonIDR), PTS: 40000, Key: false},
	}
	return stream, samples
}

func TestVerifyH264Keyframes(t *testing.T) {
	stream, samples := h264Stream()
	base, _ := timeline.NewTimeBase(90000)
	idx, err := Verify("cam", H264, stream, base, samples)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Count() != 2 {
		t.Fatalf("count = %d", idx.Count())
	}
	if !idx.Sync[0].Key {
		t.Fatal("IDR frame must be a keyframe")
	}
	if idx.Sync[1].Key {
		t.Fatal("non-IDR frame must not be a keyframe")
	}
}

func TestVerifyRejectsOutOfRangeOffset(t *testing.T) {
	stream, samples := h264Stream()
	base, _ := timeline.NewTimeBase(90000)
	samples[1].Offset = int64(len(stream) + 10)
	_, err := Verify("cam", H264, stream, base, samples)
	if err == nil {
		t.Fatal("out-of-range offset must be rejected")
	}
	if code := core.CodeOf(err); code != core.CodeBadIndex {
		t.Fatalf("code = %s, want %s", code, core.CodeBadIndex)
	}
}

func TestVerifyRejectsNegativeTimestamp(t *testing.T) {
	stream, samples := h264Stream()
	base, _ := timeline.NewTimeBase(90000)
	samples[0].PTS = -1
	_, err := Verify("cam", H264, stream, base, samples)
	if err == nil {
		t.Fatal("negative pts must be rejected")
	}
	if core.CodeOf(err) != core.CodeBadTimestamp {
		t.Fatalf("code = %s", core.CodeOf(err))
	}
}

func TestVerifyRejectsNonMonotonicPTS(t *testing.T) {
	stream, samples := h264Stream()
	base, _ := timeline.NewTimeBase(90000)
	samples[1].PTS = samples[0].PTS
	_, err := Verify("cam", H264, stream, base, samples)
	if err == nil {
		t.Fatal("equal pts must be rejected")
	}
	if core.CodeOf(err) != core.CodeBadIndex {
		t.Fatalf("code = %s", core.CodeOf(err))
	}
}

func TestVerifyRejectsNegativeByteOffset(t *testing.T) {
	stream, samples := h264Stream()
	base, _ := timeline.NewTimeBase(90000)
	samples[0].Offset = -1
	_, err := Verify("cam", H264, stream, base, samples)
	if err == nil {
		t.Fatal("negative offset must be rejected")
	}
}

func TestVerifyRejectsZeroTimescale(t *testing.T) {
	stream, samples := h264Stream()
	_, err := Verify("cam", H264, stream, timeline.TimeBase{}, samples)
	if err == nil {
		t.Fatal("unset time base must be rejected")
	}
}

func TestVerifyRejectsEmptyIndex(t *testing.T) {
	stream, _ := h264Stream()
	base, _ := timeline.NewTimeBase(90000)
	_, err := Verify("cam", H264, stream, base, nil)
	if core.CodeOf(err) != core.CodeEmptyTrack {
		t.Fatalf("code = %s", core.CodeOf(err))
	}
}

func TestSeekFindsPrecedingKeyframe(t *testing.T) {
	stream, samples := h264Stream()
	base, _ := timeline.NewTimeBase(90000)
	idx, err := Verify("cam", H264, stream, base, samples)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := idx.Seek(20000)
	if !ok || s.PTS != 0 {
		t.Fatalf("seek before first keyframe should return it, got %+v ok=%v", s, ok)
	}
	if _, ok := idx.Seek(-5); ok {
		t.Fatal("seek before stream must not fabricate a keyframe")
	}
}

func TestH265IRAPDetected(t *testing.T) {
	// NAL type 19 (IDR_W_RADL) => first byte 19<<1 = 0x26.
	idr := []byte{0, 0, 1, 19 << 1, 0, 0, 0}
	if !H265.IsKeyframe(idr) {
		t.Fatal("H.265 IRAP must be detected")
	}
	// Type 1 (TRAIL_R) => 0x02, not a keyframe.
	trail := []byte{0, 0, 1, 1 << 1, 0, 0, 0}
	if H265.IsKeyframe(trail) {
		t.Fatal("H.265 trail picture must not be a keyframe")
	}
}

func TestAV1KeyframeDetected(t *testing.T) {
	key := []byte{
		0x12, 0x00, // temporal delimiter OBU (type 2, size 0)
		0x0a, 0x02, 0x0a, 0x0b, // sequence header (type 1, size 2)
		0x32, 0x03, 0x00, 0xff, 0xff, // frame OBU type 6, KEY (frame_type 0)
	}
	if !AV1.IsKeyframe(key) {
		t.Fatal("AV1 KEY_FRAME must be detected")
	}
	inter := []byte{
		0x12, 0x00,
		0x0a, 0x02, 0x0a, 0x0b,
		0x32, 0x03, 0x20, 0xff, 0xff, // frame_type 1 = INTER
	}
	if AV1.IsKeyframe(inter) {
		t.Fatal("AV1 INTER frame must not be a keyframe")
	}
}

func TestAV1FrameWithoutSequenceHeaderRejected(t *testing.T) {
	b := []byte{0x32, 0x03, 0x00, 0xff, 0xff}
	if AV1.IsKeyframe(b) {
		t.Fatal("frame without sequence header cannot be confirmed as key")
	}
}

func TestParseFormat(t *testing.T) {
	for _, s := range []string{"h264", "h265", "av1"} {
		if _, err := ParseFormat(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if _, err := ParseFormat("vp9"); err == nil {
		t.Fatal("unknown codec must be rejected")
	}
}
