package codec_test

import (
	"errors"
	"testing"

	"mediad/internal/codec"
	"mediad/internal/mediaerr"
)

func TestRegistryNegotiatesV1AndRejectsUnknownVersions(t *testing.T) {
	r := codec.NewDefaultRegistry()
	p, err := r.Negotiate(codec.CodecH264, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.Capabilities().ParserVersion != 1 || !p.Capabilities().KeyframeDetection {
		t.Fatalf("unexpected capabilities: %+v", p.Capabilities())
	}
	_, err = r.Negotiate(codec.CodecH264, 2)
	var se *mediaerr.Error
	if !errors.As(err, &se) || se.Kind != mediaerr.KindUnknownVersion {
		t.Fatalf("err=%v want unknown_version", err)
	}
}

func TestH264CorruptPayloadUsesSharedError(t *testing.T) {
	p := codec.NewH264ParserV1()
	_, err := p.Parse([]byte{0, 0, 0, 1, 0xe5}, codec.ParseContext{PID: 256, Offset: 1880})
	var se *mediaerr.Error
	if !errors.As(err, &se) || se.Kind != mediaerr.KindCorruptPayload {
		t.Fatalf("err=%v want corrupt_payload", err)
	}
	if se.Offset != 1880 || se.PID != 256 || !se.Recoverable {
		t.Fatalf("structured context wrong: %+v", se)
	}
}

func TestAACParsesADTSHeader(t *testing.T) {
	frame := []byte{0xff, 0xf1, 0x4c, 0x80, 0x00, 0x1f, 0xfc, 1, 2, 3}
	frameLen := len(frame)
	frame[3] = 0x80 | byte((frameLen>>11)&0x03)
	frame[4] = byte((frameLen >> 3) & 0xff)
	frame[5] = byte((frameLen&0x07)<<5) | 0x1f
	p := codec.NewAACParserV1()
	samples, err := p.Parse(frame, codec.ParseContext{PID: 257, PTS: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].SampleRate != 48000 || samples[0].Channels != 2 {
		t.Fatalf("samples=%+v", samples)
	}
}
