package audio

import (
	"testing"

	"avpipe/internal/fault"
)

func testFormat(rate, channels, bits int, mask uint32) Format {
	layout, _ := LayoutForChannels(channels)
	return Format{SampleRate: rate, Channels: channels, BitsPerSample: bits,
		Layout: layout, ChannelMask: mask}
}

func TestParseRejectsTruncatedHeader(t *testing.T) {
	_, err := Parse([]byte("RIFF"), "")
	e := fault.Root(err)
	if e == nil || e.Code != fault.CodeWAVTruncated || e.Offset != 4 {
		t.Fatalf("want truncated@4, got %+v", e)
	}
}

func TestParseRejectsOutOfBoundsChunk(t *testing.T) {
	f := testFormat(48000, 2, 16, 0)
	img, err := Encode(f, []float64{0.1, -0.1}, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Claim the data chunk is much longer than the file.
	img[40] = 0xFF
	img[41] = 0x7F
	_, err = Parse(img, "")
	e := fault.Root(err)
	if e == nil || e.Code != fault.CodeWAVLength {
		t.Fatalf("want length error, got %v", err)
	}
	if !e.HasOffset || e.Offset != 44 {
		t.Fatalf("want offset 44, got %+v", e)
	}
}

func TestParseRejectsBadBlockAlign(t *testing.T) {
	f := testFormat(48000, 2, 16, 0)
	img, _ := Encode(f, make([]float64, 4), 2)
	img[32] = 3 // block align 2 -> 3
	_, err := Parse(img, "")
	if e := fault.Root(err); e == nil || e.Code != fault.CodeWAVFormat || e.Offset != 32 {
		t.Fatalf("want fmt error@32, got %+v", fault.Root(err))
	}
}

func TestParseRejectsUnsupportedDepth(t *testing.T) {
	f := testFormat(48000, 2, 16, 0)
	img, _ := Encode(f, make([]float64, 4), 2)
	img[34] = 12 // 12-bit
	_, err := Parse(img, "")
	if e := fault.Root(err); e == nil || e.Code != fault.CodeWAVFormat {
		t.Fatalf("want fmt error, got %v", err)
	}
}

func TestParseDetectsTruncatedDataPayload(t *testing.T) {
	f := testFormat(48000, 2, 16, 0)
	img, _ := Encode(f, make([]float64, 40), 20)
	short := img[:len(img)-7]
	_, err := Parse(short, "")
	if e := fault.Root(err); e == nil {
		t.Fatal("truncated payload accepted")
	}
}

func TestParseAndDecodeRoundTrip(t *testing.T) {
	for _, depth := range []int{8, 16, 24, 32} {
		f := testFormat(48000, 2, depth, 0)
		samples := make([]float64, 2*1000)
		for i := range samples {
			samples[i] = 0.5 * float64((i*7)%11-5) / 5
		}
		img, err := Encode(f, samples, 1000)
		if err != nil {
			t.Fatal(err)
		}
		info, err := Parse(img, "stereo")
		if err != nil {
			t.Fatalf("depth %d: %v", depth, err)
		}
		if info.BlockFrames != 1000 {
			t.Fatalf("depth %d frames=%d", depth, info.BlockFrames)
		}
		blocks, err := Decode(img, info, 256)
		if err != nil {
			t.Fatal(err)
		}
		if len(blocks) != 4 || blocks[3].Frames != 232 {
			t.Fatalf("depth %d block framing %d/%d", depth, len(blocks), blocks[3].Frames)
		}
		var maxErr float64
		for _, b := range blocks {
			for i, v := range b.Data {
				d := v - samples[int(b.StartFrame)*2+i]
				if d < 0 {
					d = -d
				}
				if d > maxErr {
					maxErr = d
				}
			}
		}
		limit := 1.0 / float64(int64(1)<<(depth-1))
		if maxErr > limit {
			t.Fatalf("depth %d round trip error %v > %v", depth, maxErr, limit)
		}
	}
}

func TestDecodeBlockTicksAreAbsolute(t *testing.T) {
	f := testFormat(48000, 1, 16, 0)
	img, _ := Encode(f, make([]float64, 48000), 48000)
	info, _ := Parse(img, "mono")
	blocks, _ := Decode(img, info, 1000)
	for i, b := range blocks {
		want := int64(i * 1000 * 90000 / 48000)
		if int64(b.StartTick) != want {
			t.Fatalf("block %d tick=%d want %d", i, b.StartTick, want)
		}
	}
}

func TestExtensibleMaskResolvesLayout(t *testing.T) {
	f := testFormat(48000, 6, 16, 0x3F) // 5.1 mask
	img, _ := Encode(f, make([]float64, 6*10), 10)
	info, err := Parse(img, "")
	if err != nil {
		t.Fatal(err)
	}
	if info.Format.Layout.Name != "5.1" {
		t.Fatalf("layout=%v", info.Format)
	}
}

func TestUnsupportedLayoutIsLocatable(t *testing.T) {
	_, err := ParseLayout("atmos-64")
	e := fault.Root(err)
	if e == nil || e.Code != fault.CodeUnsupportedLayout {
		t.Fatalf("want unsupported layout, got %v", err)
	}
}
