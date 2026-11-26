package format_test

import (
	"testing"

	"medialab/internal/clock"
	"medialab/internal/format"
	"medialab/internal/model"
	"medialab/internal/sample"
)

func TestMP4ParsesTrackMetadataAndRationalTimebase(t *testing.T) {
	fx := sample.BuildMP4Fixtures()
	c, err := format.ParseMP4(fx.Normal)
	if err != nil {
		t.Fatalf("ParseMP4 normal: %v", err)
	}
	if c.Format != "mp4" || len(c.Tracks) != 2 {
		t.Fatalf("container = %+v, want 2 tracks", c)
	}
	var video *format.Track
	for _, tr := range c.Tracks {
		if tr.ID == 1 {
			video = tr
		}
	}
	if video == nil {
		t.Fatal("video track 1 missing")
	}
	if video.Width != 320 || video.Height != 180 || video.Rotation != 90 {
		t.Fatalf("video metadata = %dx%d rot%d", video.Width, video.Height, video.Rotation)
	}
	if len(video.Keyframes) != 4 || video.Keyframes[0] != 1 || video.Keyframes[3] != 10 {
		t.Fatalf("keyframes = %v", video.Keyframes)
	}
	if video.Timebase() != clock.MustRat(1, 30000) {
		t.Fatalf("timebase = %v", video.Timebase())
	}
	if got := video.DurationTime().Milliseconds(); got != 12 {
		t.Fatalf("duration ms = %d, want 12", got)
	}
	if video.Crop == nil || video.Crop.Left != 8 || video.Crop.Right != 8 {
		t.Fatalf("crop = %+v", video.Crop)
	}
	if video.Color == nil || video.Color.Primaries != "bt709" {
		t.Fatalf("color = %+v", video.Color)
	}
}

func TestMP4RejectsOutOfBoundsBoxLengthWithOffset(t *testing.T) {
	// Size claims 100 bytes but only nine are present.
	data := []byte{0x00, 0x00, 0x00, 0x64, 't', 'e', 's', 't', 0x00}
	_, err := format.ParseMP4(data)
	if err == nil {
		t.Fatal("expected out-of-bounds box error")
	}
	f, ok := model.AsFault(err)
	if !ok {
		t.Fatalf("error %T is not model.Fault", err)
	}
	if f.Stage != model.StageParseMP4 || f.Offset != 0 || f.Code != "box_length_out_of_bounds" {
		t.Fatalf("fault = %+v", f)
	}
}

func TestMXFParsesKLVAndRejectsTruncatedFieldAtOriginalOffset(t *testing.T) {
	fx := sample.BuildMXFFixtures()
	c, err := format.ParseMXF(fx.Normal)
	if err != nil {
		t.Fatalf("ParseMXF normal: %v", err)
	}
	var video *format.Track
	for _, tr := range c.Tracks {
		if tr.ID == 11 {
			video = tr
		}
	}
	if video == nil || video.Width != 640 || video.Height != 360 || video.Rotation != 180 {
		t.Fatalf("MXF video = %+v", video)
	}
	if len(video.Keyframes) != 3 {
		t.Fatalf("MXF keyframes = %v", video.Keyframes)
	}

	_, err = format.ParseMXF(fx.TruncatedKLV)
	if err == nil {
		t.Fatal("expected truncated KLV rejection")
	}
	if got := model.FirstOffset(err); got != fx.TruncatedOffset {
		t.Fatalf("first_error_offset = %d, want %d (%v)", got, fx.TruncatedOffset, err)
	}
	f, _ := model.AsFault(err)
	if f.Stage != model.StageParseMXF {
		t.Fatalf("stage = %s, want parse_mxf", f.Stage)
	}
}
