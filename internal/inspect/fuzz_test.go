package inspect

import (
	"math/rand"
	"testing"

	"mediacheck/internal/model"
)

// The structural parsers read supplier-controlled bytes. They must never
// panic, index out of range or loop forever, whatever the payload.

func TestParsersSurviveRandomBytes(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	for i := 0; i < 20000; i++ {
		n := rng.Intn(64)
		b := make([]byte, n)
		rng.Read(b)
		ParseOpusDuration(b) //nolint:errcheck
		ParseADTSDuration(b) //nolint:errcheck
		H264NALTypes(b)      //nolint:errcheck
		IsH264Keyframe(b)    //nolint:errcheck
	}
}

func TestParsersSurviveAdversarialPrefixes(t *testing.T) {
	prefixes := [][]byte{
		{0xFF, 0xF1},
		{0xFF, 0xF9},
		{0x00, 0x00, 0x00},
		{0x00, 0x00, 0x00, 0x00},
		{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		{0x00, 0x00, 0x01, 0x00},
	}
	rng := rand.New(rand.NewSource(7))
	for _, p := range prefixes {
		for i := 0; i < 2000; i++ {
			b := append([]byte(nil), p...)
			tail := make([]byte, rng.Intn(32))
			rng.Read(tail)
			b = append(b, tail...)
			ParseOpusDuration(b) //nolint:errcheck
			ParseADTSDuration(b) //nolint:errcheck
			H264NALTypes(b)      //nolint:errcheck
		}
	}
}

func TestInspectorSurvivesCorruptPackets(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	in := New()
	for i := 0; i < 5000; i++ {
		b := make([]byte, rng.Intn(40))
		rng.Read(b)
		track := model.TrackAudio
		codec := model.CodecOpus
		if i%2 == 0 {
			track, codec = model.TrackVideo, model.CodecH264
		}
		in.Inspect(model.Packet{
			Track: track, Codec: codec, Sequence: uint32(i), Duration: int64(rng.Intn(3000) + 1),
			Slot: model.Slot{Start: int64(i) * 1000, End: int64(i)*1000 + int64(rng.Intn(1000)+1)},
			Data: b,
		})
	}
	res := in.Finish()
	if len(res.Faults) == 0 {
		t.Fatal("random payloads produced no faults, parser may not be exercised")
	}
}

func TestAVCCLengthCannotOverrun(t *testing.T) {
	// A length prefix of 0xFFFFFFFF must be rejected, not wrap into an
	// in-range slice.
	au := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x65, 0x00}
	if _, err := H264NALTypes(au); err == nil {
		t.Fatal("huge NAL length accepted")
	}
	// Zero-length NAL must be rejected.
	if _, err := H264NALTypes([]byte{0, 0, 0, 0}); err == nil {
		t.Fatal("zero-length NAL accepted")
	}
}
