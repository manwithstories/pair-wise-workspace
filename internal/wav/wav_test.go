package wav

import (
	"testing"

	"avcheck/internal/fault"
)

func TestWAVRoundTrip(t *testing.T) {
	in := &PCM{Rate: 48000, Layout: Stereo, Samples: []int16{1, -1, 300, -300, 0, 0}}
	out, err := DecodeWAV(EncodeWAV(in))
	if err != nil {
		t.Fatal(err)
	}
	if out.Rate != 48000 || out.Layout != Stereo || len(out.Samples) != 6 {
		t.Fatalf("bad round trip: %+v", out)
	}
	for i := range in.Samples {
		if in.Samples[i] != out.Samples[i] {
			t.Fatalf("sample %d differs", i)
		}
	}
}

func TestRejectTruncatedHeader(t *testing.T) {
	if _, err := DecodeWAV([]byte("RIFF")); !fault.IsCode(err, fault.Unsupported) {
		t.Fatalf("want unsupported, got %v", err)
	}
	bad := EncodeWAV(&PCM{Rate: 48000, Layout: Mono, Samples: []int16{1, 2}})
	if _, err := DecodeWAV(bad[:20]); !fault.IsCode(err, fault.Unsupported) {
		t.Fatalf("want unsupported for short file, got %v", err)
	}
}

func TestRejectNonIntegralFrames(t *testing.T) {
	enc := EncodeWAV(&PCM{Rate: 48000, Layout: Stereo, Samples: []int16{1, 2, 3}}) // 1.5 frames
	if _, err := DecodeWAV(enc); !fault.IsCode(err, fault.Unsupported) {
		t.Fatalf("want unsupported, got %v", err)
	}
}

func TestBlockRoundTripAndDigest(t *testing.T) {
	b := &Block{Seq: 7, Track: 1, Rate: 44100, Layout: Quad,
		Samples: []int16{1, 2, 3, 4},
		Red: [2]Redundancy{
			{Seq: 6, Samples: []int16{9, 9, 9, 9}},
			{Seq: 5, Samples: []int16{8, 8, 8, 8}},
		}}
	got, err := DecodeBlock(EncodeBlock(b))
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != 7 || got.Layout != Quad || got.Red[1].Seq != 5 || got.Red[0].Samples[0] != 9 {
		t.Fatalf("bad block round trip: %+v", got)
	}
	wire := EncodeBlock(b)
	wire[30] ^= 0xff // corrupt payload
	if _, err := DecodeBlock(wire); !fault.IsCode(err, fault.Corrupt) {
		t.Fatalf("want corrupt on digest mismatch, got %v", err)
	}
}

func TestUnknownLayoutLocatable(t *testing.T) {
	_, err := ParseLayout("7.1.4")
	var e *fault.Error
	if !fault.IsCode(err, fault.Unsupported) {
		t.Fatalf("want unsupported, got %v", err)
	}
	if fault.IsCode(err, fault.Corrupt) {
		t.Fatal("must not be classified corrupt")
	}
	_ = e
	if _, err := LayoutFromTag(0x7f); !fault.IsCode(err, fault.Unsupported) {
		t.Fatalf("tag byte: want unsupported, got %v", err)
	}
}

func TestMatrixShapes(t *testing.T) {
	for _, l := range []Layout{Mono, Stereo, Quad, Surround51} {
		if len(l.Matrix()) != l.Channels() {
			t.Fatalf("%v matrix shape", l)
		}
	}
}
