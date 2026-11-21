package wav

import (
	"bytes"
	"errors"
	"testing"

	"avcheck/internal/clock"
)

func makePCM(channels, frames int, bits int) *PCM {
	p := &PCM{Channels: channels, BitsPerSamp: bits, SampleRate: clock.SampleRate}
	p.Samples = make([]int32, frames*channels)
	for i := range p.Samples {
		p.Samples[i] = int32((i*97)%30000 - 15000)
	}
	return p
}

func TestRoundTrip16And24Bit(t *testing.T) {
	for _, bits := range []int{16, 24} {
		for _, ch := range []int{1, 2, 8} {
			src := makePCM(ch, 32, bits)
			got, err := Decode(bytes.NewReader(Encode(src, bits)))
			if err != nil {
				t.Fatalf("bits=%d ch=%d decode: %v", bits, ch, err)
			}
			if got.Channels != ch || got.BitsPerSamp != bits || got.Frames() != 32 {
				t.Fatalf("header mismatch: %+v", got)
			}
			for i, v := range src.Samples {
				// 24-bit encoding preserves every value inside the signed 24-bit range.
				if got.Samples[i] != v {
					t.Fatalf("bits=%d sample[%d]=%d want %d", bits, i, got.Samples[i], v)
				}
			}
		}
	}
}

func TestRejectTruncatedHeader(t *testing.T) {
	src := Encode(makePCM(2, 16, 16), 16)
	for _, cut := range []int{4, 10, 20, len(src) - 7} {
		_, err := Decode(bytes.NewReader(src[:cut]))
		var ce *clock.Error
		if !errors.As(err, &ce) || ce.Kind != clock.KindTruncatedWAV {
			t.Fatalf("cut=%d err=%v, want truncated_wav", cut, err)
		}
		if ce.Offset < 0 || ce.Offset > int64(len(src)) {
			t.Fatalf("cut=%d offset=%d not in range", cut, ce.Offset)
		}
	}
}

func TestRejectIllegalChunkLength(t *testing.T) {
	src := Encode(makePCM(2, 16, 16), 16)
	// Corrupt the data chunk size (offset 40: RIFF 12 + fmt header 8 + fmt 16).
	src[40] = 0xFF
	src[41] = 0xFF
	src[42] = 0xFF
	src[43] = 0x7F
	_, err := Decode(bytes.NewReader(src))
	var ce *clock.Error
	if !errors.As(err, &ce) || ce.Kind != clock.KindTruncatedWAV {
		t.Fatalf("err=%v, want chunk-exceeds-container truncated error", err)
	}
}

func TestRejectRateMismatch(t *testing.T) {
	src := Encode(makePCM(1, 8, 16), 16)
	// fmt layout at 20: audioFmt(2) channels(2) rate(4 @24) byteRate(4 @28)
	// blockAlign(2 @32) bits(2 @34). Rewrite rate and byteRate consistently.
	src[24] = 0x40
	src[25] = 0xBB // 48000 -> 47936
	src[28] = 0x40
	src[29] = 0xBB // mono, 16-bit -> byte rate equals sample rate
	_, err := Decode(bytes.NewReader(src))
	var ce *clock.Error
	if !errors.As(err, &ce) || ce.Kind != clock.KindRateMismatch {
		t.Fatalf("err=%v, want rate_mismatch at offset 24", err)
	}
	if ce.Offset != 24 {
		t.Fatalf("offset=%d, want 24", ce.Offset)
	}
}

func TestRejectBadBlockAlign(t *testing.T) {
	src := Encode(makePCM(2, 8, 16), 16)
	// blockAlign at offset 32; fix byteRate at 28 to rate*3 so the align check
	// fires before the byte-rate consistency check.
	src[32] = 3
	src[28] = 0xC0
	src[29] = 0x17
	src[30] = 0x02
	src[31] = 0x00 // 144000 = 48000 * 3
	_, err := Decode(bytes.NewReader(src))
	var ce *clock.Error
	if !errors.As(err, &ce) || ce.Kind != clock.KindBadBlock {
		t.Fatalf("err=%v, want bad_block", err)
	}
}

func TestRejectDataNotBlockAligned(t *testing.T) {
	src := Encode(makePCM(2, 8, 16), 16)
	// Shrink declared data length by one byte.
	src[40]--
	_, err := Decode(bytes.NewReader(src))
	var ce *clock.Error
	if !errors.As(err, &ce) || ce.Kind != clock.KindBadBlock {
		t.Fatalf("err=%v, want bad_block for unaligned data", err)
	}
}
