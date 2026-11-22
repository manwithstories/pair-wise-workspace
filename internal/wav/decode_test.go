package wav

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"mediakernel/internal/model"
)

func TestRoundTripAndBlockAlignment(t *testing.T) {
	samples := SineFrames(48000, 2, 128, 440)
	raw, err := WriteBytes(48000, 2, samples)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewDecoder(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Format.SampleRate != 48000 || dec.Format.Channels != 2 || dec.Format.BitsPerSample != 16 {
		t.Fatalf("format mismatch: %+v", dec.Format)
	}
	if dec.Format.BlockAlign() != 4 || dec.Format.FrameCount() != 128 {
		t.Fatalf("alignment mismatch: %+v", dec.Format)
	}
	var got []byte
	for {
		block, err := dec.NextBlock(32)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(block)%4 != 0 {
			t.Fatal("block not aligned")
		}
		got = append(got, block...)
	}
	if len(got) != len(samples)*2 {
		t.Fatalf("pcm length: got %d want %d", len(got), len(samples)*2)
	}
}

func TestTruncatedHeader(t *testing.T) {
	_, err := NewDecoder(bytes.NewReader([]byte("RIFFxxxx")))
	var me *model.MediaError
	if !errors.As(err, &me) || me.Category != model.CatTruncatedInput {
		t.Fatalf("want truncated input, got %v", err)
	}
}

func TestHalfFrameRejected(t *testing.T) {
	samples := SineFrames(16000, 1, 4, 1000)
	raw, _ := WriteBytes(16000, 1, samples)
	// Keep the declared data size (8 bytes) but cut the final sample byte so
	// the physical stream ends on a half frame.
	raw = raw[:len(raw)-1]
	dec, err := NewDecoder(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var sawHalf bool
	for {
		_, err := dec.NextBlock(2)
		if errors.Is(err, io.EOF) {
			break
		}
		if err == nil {
			continue
		}
		var me *model.MediaError
		if errors.As(err, &me) && me.Category == model.CatTruncatedInput {
			sawHalf = true
			break
		}
		t.Fatalf("unexpected: %v", err)
	}
	if !sawHalf {
		t.Fatal("half frame was silently discarded")
	}
}

func TestBadBitDepthAndRiff(t *testing.T) {
	if _, err := NewDecoder(bytes.NewReader([]byte("not-a-wav-file!!"))); err == nil {
		t.Fatal("non-wav input accepted")
	}
	// 8-bit PCM must be rejected.
	good, _ := WriteBytes(8000, 1, SineFrames(8000, 1, 4, 200))
	raw := make([]byte, len(good))
	copy(raw, good)
	raw[34] = 8 // bits per sample field
	if _, err := NewDecoder(bytes.NewReader(raw)); err == nil {
		t.Fatal("8-bit PCM accepted")
	}
}

func TestMaliciousLengthRejected(t *testing.T) {
	header := []byte("RIFF")
	header = append(header, 0, 0, 0, 0)
	header = append(header, "WAVE"...)
	header = append(header, "fmt "...)
	header = append(header, 0xFF, 0xFF, 0xFF, 0x7F) // huge chunk length
	if _, err := NewDecoder(bytes.NewReader(header)); err == nil {
		t.Fatal("huge chunk length accepted")
	}
}
