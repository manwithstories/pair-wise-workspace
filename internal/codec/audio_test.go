package codec

import (
	"math"
	"testing"

	"mediacore/internal/media"
)

// TestPCMDescribe recognises the sample entries the container can produce.
func TestPCMDescribe(t *testing.T) {
	reg := Default()
	for _, tc := range []struct {
		tag  string
		want Mapping
	}{
		{"sowt", MappingS16LE},
		{"twos", MappingS16BE},
		{"lpcm", MappingS16LE},
	} {
		desc, err := reg.Describe(tc.tag, nil, 48000, 2, 16)
		if err != nil {
			t.Fatalf("%s: %v", tc.tag, err)
		}
		if desc.Format.Mapping != tc.want || desc.Format.Channels != 2 || desc.Format.FrameBytes != 4 {
			t.Fatalf("%s: %+v", tc.tag, desc.Format)
		}
	}
}

// TestPCMDecode checks the exact scale and endianness of 16-bit PCM.
func TestPCMDecode(t *testing.T) {
	pcm := &PCM{}
	f := Format{Codec: "pcm", Mapping: MappingS16LE, SampleRate: 48000, Channels: 2, FrameBytes: 4}
	// 0, 32767, -32768, 16384 little-endian.
	sample := []byte{0x00, 0x00, 0xff, 0x7f, 0x00, 0x80, 0x00, 0x40}
	dst := make([]float64, 4)
	n, err := pcm.Decode(sample, f, dst)
	if err != nil || n != 2 {
		t.Fatalf("decode: n=%d err=%v", n, err)
	}
	if dst[0] != 0 {
		t.Errorf("0 -> %v", dst[0])
	}
	if math.Abs(dst[1]-32767.0/32768.0) > 1e-12 {
		t.Errorf("max -> %v", dst[1])
	}
	if dst[2] != -1 {
		t.Errorf("min -> %v", dst[2])
	}
	if dst[3] != 0.5 {
		t.Errorf("0x4000 -> %v", dst[3])
	}
}

// TestPCMBigEndian checks byte-order handling.
func TestPCMBigEndian(t *testing.T) {
	pcm := &PCM{}
	f := Format{Codec: "pcm", Mapping: MappingS16BE, Channels: 1, FrameBytes: 2}
	dst := make([]float64, 1)
	if _, err := pcm.Decode([]byte{0x7f, 0xff}, f, dst); err != nil {
		t.Fatal(err)
	}
	if math.Abs(dst[0]-32767.0/32768.0) > 1e-12 {
		t.Fatalf("max -> %v", dst[0])
	}
}

// TestPCMUnaligned reports a partial frame rather than silently dropping it.
func TestPCMUnaligned(t *testing.T) {
	pcm := &PCM{}
	f := Format{Codec: "pcm", Mapping: MappingS16LE, Channels: 2, FrameBytes: 4}
	_, err := pcm.Decode([]byte{1, 2, 3}, f, make([]float64, 4))
	if err == nil {
		t.Fatal("expected UNALIGNED_PCM")
	}
	if me, _ := media.AsError(err); me == nil || me.Code != media.CodeUnalignedPCM {
		t.Fatalf("got %v", err)
	}
}

// TestPCMRejectsBadGeometry refuses a width or channel count the engine
// cannot render.
func TestPCMRejectsBadGeometry(t *testing.T) {
	reg := Default()
	if _, err := reg.Describe("sowt", nil, 48000, 6, 16); err == nil {
		t.Fatal("6 channels accepted")
	}
	if _, err := reg.Describe("sowt", nil, 48000, 2, 24); err == nil {
		t.Fatal("24-bit accepted")
	}
	if _, err := reg.Describe("sowt", nil, 0, 2, 16); err == nil {
		t.Fatal("zero rate accepted")
	}
}

// TestAACAudioSpecificConfig checks a standard AAC-LC 48 kHz stereo config.
func TestAACAudioSpecificConfig(t *testing.T) {
	reg := Default()
	// AOT 2, index 3 (48 kHz), channel config 2:
	// 00010 0011 0010 000 -> 0x11 0x90
	desc, err := reg.Describe("mp4a", []byte{0x11, 0x90}, 48000, 2, 16)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	f := desc.Format
	if f.Codec != "aac" || f.SampleRate != 48000 || f.Channels != 2 || f.ObjectType != 2 {
		t.Fatalf("format %+v", f)
	}
	if f.Mapping != MappingCompressed {
		t.Fatalf("mapping %s", f.Mapping)
	}
}

// TestAACExplicitRate exercises the 0x0f escape form.
func TestAACExplicitRate(t *testing.T) {
	reg := Default()
	// AOT 2 (00010), idx 15 (1111), 24-bit rate 48000 (0x00BB80), ch 1.
	// bits: 00010 1111 000000001011101110000000 0001
	cfg := bitsToBytes("00010" + "1111" + "000000001011101110000000" + "0001")
	desc, err := reg.Describe("mp4a", cfg, 44100, 1, 16)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if desc.Format.SampleRate != 48000 || desc.Format.Channels != 1 {
		t.Fatalf("format %+v", desc.Format)
	}
	if len(desc.Warnings) == 0 {
		t.Error("expected a warning about the stale sample-entry rate")
	}
}

// TestAACRejectsNonLCMakes an unsupported profile a typed input error.
func TestAACRejectsNonLC(t *testing.T) {
	reg := Default()
	// AOT 5 (SBR) -> 00101 0011 0010 000
	cfg := bitsToBytes("00101" + "0011" + "0010" + "000")
	_, err := reg.Describe("mp4a", cfg, 48000, 2, 16)
	if err == nil {
		t.Fatal("AOT 5 accepted")
	}
	if me, _ := media.AsError(err); me == nil || me.Code != media.CodeUnsupportedCodec {
		t.Fatalf("got %v", err)
	}
}

// TestAACDecodeRefused proves compressed payloads fail loudly.
func TestAACDecodeRefused(t *testing.T) {
	a := &AAC{}
	_, err := a.Decode([]byte{1, 2, 3}, Format{Codec: "aac", Mapping: MappingCompressed}, nil)
	if me, _ := media.AsError(err); me == nil || me.Code != media.CodeUnsupportedCodec {
		t.Fatalf("got %v", err)
	}
}

// TestUnknownEntry rejects a tag no adapter claims.
func TestUnknownEntry(t *testing.T) {
	_, err := Default().Describe("avc1", nil, 48000, 2, 16)
	if me, _ := media.AsError(err); me == nil || me.Code != media.CodeUnsupportedCodec {
		t.Fatalf("got %v", err)
	}
}

// TestVersionString is part of the checkpoint binding, so it must be stable.
func TestVersionString(t *testing.T) {
	if got := PCMVersion.String(); got != "pcm/1.0" {
		t.Fatalf("got %q", got)
	}
	if got := AACVersion.String(); got != "aac/1.0" {
		t.Fatalf("got %q", got)
	}
}

func bitsToBytes(bits string) []byte {
	out := make([]byte, (len(bits)+7)/8)
	for i := 0; i < len(bits); i++ {
		if bits[i] == '1' {
			out[i/8] |= 1 << (7 - uint(i%8))
		}
	}
	return out
}
