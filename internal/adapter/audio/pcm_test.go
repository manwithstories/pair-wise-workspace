package audio

import (
	"encoding/binary"
	"testing"

	"mediacheck/internal/model"
)

func makePCM16(samples ...int16) []byte {
	b := make([]byte, len(samples)*2)
	for i, v := range samples {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}

func TestDecodeEncodeRoundTrip(t *testing.T) {
	dec, err := NewDecoder(model.TrackMeta{Codec: "ipcm", BitDepth: 16, Layout: model.LayoutStereo})
	if err != nil {
		t.Fatal(err)
	}
	payload := makePCM16(100, -200, 0, 0, 32767, -32768)
	frames, st, err := dec.Decode(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 || frames[0].Samples[0] != 100 || frames[0].Samples[1] != -200 {
		t.Fatalf("frames = %+v", frames)
	}
	if st.ClippedSamples != 2 { // +32767 与 -32768 触顶/触底
		t.Fatalf("clipped = %d", st.ClippedSamples)
	}
	if st.SilentFrames != 1 { // 第二帧 (0,0)
		t.Fatalf("silent = %d", st.SilentFrames)
	}
	out := Encode16(frames, 2)
	if len(out) != len(payload) {
		t.Fatalf("encoded len = %d want %d", len(out), len(payload))
	}
}

func TestDecode24BitSignExtension(t *testing.T) {
	dec, err := NewDecoder(model.TrackMeta{Codec: "ipcm", BitDepth: 24, Layout: model.LayoutStereo})
	if err != nil {
		t.Fatal(err)
	}
	// 单声道两帧需要 2 声道布局；这里构造 1 帧立体声：负值 + 正值。
	payload := []byte{
		0x00, 0x00, 0x80, // -8388608 (L)
		0xff, 0xff, 0x7f, // +8388607 (R)
	}
	frames, _, err := dec.Decode(payload)
	if err != nil {
		t.Fatal(err)
	}
	if frames[0].Samples[0] != -(1<<23) || frames[0].Samples[1] != (1<<23)-1 {
		t.Fatalf("24bit sign extend failed: %+v", frames[0])
	}
}

func TestDecodeRejectsMisalignedAndUnknown(t *testing.T) {
	dec, _ := NewDecoder(model.TrackMeta{Codec: "ipcm", BitDepth: 16, Layout: model.LayoutStereo})
	if _, _, err := dec.Decode([]byte{1, 2, 3}); err == nil {
		t.Fatal("misaligned payload must fail")
	}
	if _, err := NewDecoder(model.TrackMeta{Codec: "mp4a", BitDepth: 16, Layout: model.LayoutStereo}); err == nil {
		t.Fatal("non-ipcm codec must be rejected")
	}
	if _, err := NewDecoder(model.TrackMeta{Codec: "ipcm", BitDepth: 16, Layout: "weird"}); err == nil {
		t.Fatal("unknown layout must be rejected")
	}
}

func TestEncode16ClampsOverflow(t *testing.T) {
	frames := []Frame{{Samples: []int32{40000, -40000}}}
	out := Encode16(frames, 2)
	lo := int16(binary.LittleEndian.Uint16(out[0:2]))
	hi := int16(binary.LittleEndian.Uint16(out[2:4]))
	if lo != 32767 || hi != -32768 {
		t.Fatalf("clamp failed: %d %d", lo, hi)
	}
}
