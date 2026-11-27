package normalize

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"avflow/internal/model"
	"avflow/internal/stream"
)

func audioTrack(id uint32, rate uint32, layout, channels uint8) stream.TrackDesc {
	return stream.TrackDesc{ID: id, Kind: stream.KindAudio,
		TimeBase: model.Rational{Num: 1, Den: int64(rate)},
		Channels: channels, ChannelLayout: layout, SampleRate: rate, Name: "aud"}
}

func pcmPacket(seq uint64, pts int64, channels uint8, vals ...int16) *stream.Packet {
	payload := make([]byte, len(vals)*2)
	for i, v := range vals {
		payload[i*2] = byte(uint16(v))
		payload[i*2+1] = byte(uint16(v) >> 8)
	}
	return &stream.Packet{Track: 1, Seq: seq, PTS: pts, Payload: payload,
		ChecksumOK: true, PTSNS: model.ToNanoseconds(pts, model.Rational{Num: 1, Den: 48000})}
}

func TestMonoToStereo48k(t *testing.T) {
	desc := audioTrack(1, 48000, stream.LayoutMono, 1)
	pkts := []*stream.Packet{pcmPacket(0, 0, 1, 16000, -16000)}
	res, err := NormalizeAudio(desc, pkts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Frames) != 1 || res.TotalSamples != 2 {
		t.Fatalf("frames=%d samples=%d", len(res.Frames), res.TotalSamples)
	}
	s := res.Frames[0].Samples
	if s[0] != s[1] {
		t.Fatal("mono must be duplicated to L and R")
	}
	if s[0] < 0.48 || s[0] > 0.49 {
		t.Fatalf("level %f out of range", s[0])
	}
}

func TestResample44100To48000(t *testing.T) {
	desc := audioTrack(1, 44100, stream.LayoutStereo, 2)
	// One full second of silence-ish constant input: 44100 stereo frames.
	vals := make([]int16, 44100*2)
	for i := range vals {
		vals[i] = 1000
	}
	payload := make([]byte, len(vals)*2)
	for i, v := range vals {
		payload[i*2] = byte(uint16(v))
		payload[i*2+1] = byte(uint16(v) >> 8)
	}
	p := &stream.Packet{Track: 1, Seq: 0, Payload: payload, ChecksumOK: true}
	res, err := NormalizeAudio(desc, []*stream.Packet{p})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.TotalSamples; got != 48000 {
		t.Fatalf("output samples = %d, want 48000", got)
	}
	// Determinism: run twice and compare bytes.
	res2, _ := NormalizeAudio(desc, []*stream.Packet{p})
	if len(res2.Frames[0].Samples) != len(res.Frames[0].Samples) {
		t.Fatal("non-deterministic sample count")
	}
	for i := range res.Frames[0].Samples {
		if res.Frames[0].Samples[i] != res2.Frames[0].Samples[i] {
			t.Fatal("non-deterministic resampling")
		}
	}
}

func TestSurroundDownmix(t *testing.T) {
	for _, tc := range []struct {
		layout   uint8
		channels uint8
	}{
		{stream.Layout51, 6}, {stream.Layout71, 8},
	} {
		desc := audioTrack(1, 48000, tc.layout, tc.channels)
		vals := make([]int16, tc.channels)
		for i := range vals {
			vals[i] = 8000
		}
		res, err := NormalizeAudio(desc, []*stream.Packet{pcmPacket(0, 0, tc.channels, vals...)})
		if err != nil {
			t.Fatalf("layout %d: %v", tc.layout, err)
		}
		if got := len(res.Frames[0].Samples); got != 2 {
			t.Fatalf("layout %d produced %d outputs, want 2", tc.layout, got)
		}
	}
}

func TestChannelAndRateConflict(t *testing.T) {
	bad := audioTrack(1, 48000, stream.Layout51, 2) // layout says 6, desc says 2
	if _, err := NormalizeAudio(bad, nil); err == nil {
		t.Fatal("channel/layout conflict not rejected")
	}
	zero := audioTrack(1, 0, stream.LayoutStereo, 2)
	if _, err := NormalizeAudio(zero, nil); err == nil {
		t.Fatal("zero sample rate not rejected")
	}
	// Ragged payload for a declared stereo track is a recoverable fault.
	good := audioTrack(1, 48000, stream.LayoutStereo, 2)
	p := pcmPacket(0, 0, 2, 1, 2, 3) // 3 samples, not divisible by 2
	res, err := NormalizeAudio(good, []*stream.Packet{p})
	if err != nil || len(res.Faults) != 1 || res.Faults[0].Type != model.FaultChannelConflict {
		t.Fatalf("ragged payload: err=%v faults=%v", err, res.Faults)
	}
}

func TestAudioBoundaryDrift(t *testing.T) {
	desc := audioTrack(1, 48000, stream.LayoutStereo, 2)
	// 12345 ns is not on the 48k sample grid.
	p := &stream.Packet{Track: 1, Seq: 0, PTSNS: 12345, EndPTSNS: 99999,
		Payload: []byte{0, 0, 0, 0}, ChecksumOK: true}
	res, _ := NormalizeAudio(desc, []*stream.Packet{p})
	if len(res.Faults) != 1 || res.Faults[0].Type != model.FaultBoundaryDrift {
		t.Fatalf("want boundary drift fault, got %v", res.Faults)
	}
}

func pngPayload(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func videoTrack(id uint32, tag uint8) stream.TrackDesc {
	return stream.TrackDesc{ID: id, Kind: stream.KindVideo,
		TimeBase: model.Rational{Num: 1, Den: 90000}, ColorTag: tag,
		Width: 200, Height: 100, MaxKeyIntervalNS: 1_000_000_000, Name: "vid"}
}

func TestVideoResizeDeterministic(t *testing.T) {
	desc := videoTrack(1, stream.ColorSRGB)
	png := pngPayload(t, 200, 100)
	p := &stream.Packet{Track: 1, Seq: 0, Payload: png, ChecksumOK: true}
	res, err := NormalizeVideo(desc, []*stream.Packet{p})
	if err != nil || len(res.Frames) != 1 {
		t.Fatalf("err=%v faults=%v", err, res.Faults)
	}
	f := res.Frames[0]
	if f.Width > CanonicalFrameW || f.Height > CanonicalFrameH {
		t.Fatalf("frame not fitted: %dx%d", f.Width, f.Height)
	}
	res2, _ := NormalizeVideo(desc, []*stream.Packet{p})
	if !bytes.Equal(res.Frames[0].PNG, res2.Frames[0].PNG) {
		t.Fatal("video normalization is not deterministic")
	}
}

func TestColorTagConflictAndUnknown(t *testing.T) {
	desc := videoTrack(1, stream.ColorSRGB)
	png := pngPayload(t, 32, 24)
	conflict := &stream.Packet{Track: 1, Seq: 0, ColorTag: stream.ColorBT709,
		Payload: png, ChecksumOK: true}
	res, err := NormalizeVideo(desc, []*stream.Packet{conflict})
	if err != nil || len(res.Faults) != 1 || res.Faults[0].Type != model.FaultColorConflict {
		t.Fatalf("conflict: err=%v faults=%v", err, res.Faults)
	}
	if res.Frames[0].ColorTag != stream.ColorSRGB {
		t.Fatal("conflicting frame must retain track profile")
	}
	unknown := &stream.Packet{Track: 1, Seq: 1, ColorTag: stream.ColorUnknown,
		Payload: png, ChecksumOK: true}
	res2, _ := NormalizeVideo(desc, []*stream.Packet{unknown})
	if len(res2.Faults) != 1 || res2.Faults[0].Type != model.FaultUnknownTag {
		t.Fatalf("unknown tag: faults=%v", res2.Faults)
	}
	if res2.Faults[0].Track != 1 || res2.Faults[0].PacketSeq != 1 {
		t.Fatal("unknown-tag fault must carry track and packet position")
	}
}
