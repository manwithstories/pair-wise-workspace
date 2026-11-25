package ingest_test

import (
	"errors"
	"testing"

	"mediacheck/internal/fixture"
	"mediacheck/internal/ingest"
	"mediacheck/internal/media"
)

func TestParseH264FramesAndKeyframes(t *testing.T) {
	b := fixture.Normal()
	units, err := ingest.ParseElementary(ingest.ParseRequest{
		Codec: b.Codec, Data: b.Video, Clock: b.Clock, FrameDuration: b.FrameDur,
	})
	if err != nil {
		t.Fatal(err)
	}
	frames := 0
	keys := 0
	for _, u := range units {
		if u.Duration == 0 {
			continue
		}
		frames++
		if u.Keyframe {
			keys++
		}
		if u.Offset < 0 || int(u.Offset) >= len(b.Video) {
			t.Fatalf("bad offset %d", u.Offset)
		}
	}
	if frames != fixture.FrameCount || keys != 2 {
		t.Fatalf("frames=%d keys=%d", frames, keys)
	}
}

func TestTruncatedNALAndOBUHaveOffsets(t *testing.T) {
	clock := media.DefaultClock()
	_, err := ingest.ParseElementary(ingest.ParseRequest{
		Codec: ingest.CodecH264, Data: fixture.TruncatedH264(), Clock: &clock, FrameDuration: 3000,
	})
	assertKind(t, err, media.KindTruncated)

	_, err = ingest.ParseElementary(ingest.ParseRequest{
		Codec: ingest.CodecAV1, Data: fixture.TruncatedAV1(), Clock: &clock, FrameDuration: 3000,
	})
	me := assertKind(t, err, media.KindTruncated)
	if me.Offset != 0 {
		t.Fatalf("OBU offset=%d want 0", me.Offset)
	}
}

func TestRejectUnknownCodecAndMissingClock(t *testing.T) {
	clock := media.DefaultClock()
	_, err := ingest.ParseElementary(ingest.ParseRequest{
		Codec: "vp9", Data: []byte{1, 2}, Clock: &clock,
	})
	assertKind(t, err, media.KindUnknownCodec)

	_, err = ingest.ParseElementary(ingest.ParseRequest{
		Codec: ingest.CodecAV1, Data: fixture.TruncatedAV1(),
	})
	assertKind(t, err, media.KindMissingClock)
}

func TestPCMRejectsTruncatedFrame(t *testing.T) {
	clock := media.DefaultClock()
	_, err := ingest.ReadS16LE([]byte{1, 2, 3}, ingest.PCMFormat{SampleRate: 48000, Channels: 1}, &clock, 0)
	me := assertKind(t, err, media.KindTruncated)
	if me.Offset != 2 {
		t.Fatalf("offset=%d want 2", me.Offset)
	}
}

func assertKind(t *testing.T, err error, kind media.Kind) *media.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error", kind)
	}
	var me *media.Error
	if !errors.As(err, &me) {
		t.Fatalf("non-media error: %v", err)
	}
	if me.Kind != kind {
		t.Fatalf("kind=%s want %s err=%v", me.Kind, kind, err)
	}
	return me
}
