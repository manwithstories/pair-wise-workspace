package wav_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"media-pipeline/internal/fault"
	"media-pipeline/internal/wav"
)

// buildWAV 构造一个最小合法 PCM/WAV，pcm 为交织整数采样。
func buildWAV(sampleRate uint32, channels, bits uint16, pcm []byte) []byte {
	blockAlign := channels * bits / 8
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))                 // PCM
	binary.Write(&b, binary.LittleEndian, channels)
	binary.Write(&b, binary.LittleEndian, sampleRate)
	binary.Write(&b, binary.LittleEndian, sampleRate*uint32(blockAlign))
	binary.Write(&b, binary.LittleEndian, blockAlign)
	binary.Write(&b, binary.LittleEndian, bits)
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

func decode(t *testing.T, raw []byte) (*wav.Header, []byte) {
	t.Helper()
	h, err := wav.DecodeHeader("w1", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	pcm := raw[h.DataStart : h.DataStart+int64(h.DataSize)]
	return h, pcm
}

func TestValidWAV(t *testing.T) {
	pcm := make([]byte, 4*10) // 10 帧立体声 16bit
	raw := buildWAV(48000, 2, 16, pcm)
	h, gotPCM := decode(t, raw)
	if h.Format.SampleRate != 48000 || h.Format.Channels != 2 || h.Format.BitsPerSample != 16 {
		t.Fatalf("format = %+v", h.Format)
	}
	frames, err := h.CheckPCM("w1", gotPCM)
	if err != nil || frames != 10 {
		t.Fatalf("frames = (%d,%v), want 10", frames, err)
	}
}

func TestBadMagic(t *testing.T) {
	raw := buildWAV(48000, 1, 16, make([]byte, 4))
	raw[8] = 'X' // 破坏 WAVE 魔数，偏移 8
	_, err := wav.DecodeHeader("w1", bytes.NewReader(raw))
	fe, ok := fault.As(err)
	if !ok || fe.ByteOffset != 8 {
		t.Fatalf("magic err = %v, want byte offset 8", err)
	}
}

func TestRiffLengthTruncation(t *testing.T) {
	raw := buildWAV(48000, 1, 16, make([]byte, 8))
	// RIFF 声明比实际文件大，截断点就是文件末尾。
	binary.LittleEndian.PutUint32(raw[4:8], uint32(len(raw)-1)+100)
	_, err := wav.DecodeHeader("w1", bytes.NewReader(raw))
	fe, ok := fault.As(err)
	if !ok || fe.ByteOffset != int64(len(raw)) {
		t.Fatalf("truncation err = %v, want offset %d", err, len(raw))
	}
}

func TestChunkOrderDataBeforeFmt(t *testing.T) {
	pcm := make([]byte, 4)
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)-8))
	b.WriteString("WAVE")
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	b.WriteString("fmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	b.Write(make([]byte, 16))
	_, err := wav.DecodeHeader("w1", bytes.NewReader(b.Bytes()))
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindMalformedInput {
		t.Fatalf("order err = %v, want malformed input", err)
	}
}

func TestBlockAlignMismatch(t *testing.T) {
	raw := buildWAV(48000, 2, 16, make([]byte, 8))
	// 块对齐字段位于 fmt 载荷偏移 12，即文件偏移 20+12=32。
	binary.LittleEndian.PutUint16(raw[32:34], 3)
	_, err := wav.DecodeHeader("w1", bytes.NewReader(raw))
	fe, ok := fault.As(err)
	if !ok || fe.ByteOffset != 32 {
		t.Fatalf("align err = %v, want byte 32", err)
	}
}

func TestNonFrameAlignedTail(t *testing.T) {
	raw := buildWAV(48000, 2, 16, make([]byte, 9)) // 非整帧
	h, err := wav.DecodeHeader("w1", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("header decode: %v", err)
	}
	pcm := raw[h.DataStart : h.DataStart+int64(h.DataSize)]
	_, err = h.CheckPCM("w1", pcm)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindMalformedInput {
		t.Fatalf("check err = %v, want malformed", err)
	}
	// 末帧起始：dataStart+8，采样序号 8/4=2
	if fe.SampleOffset != 2 || fe.ByteOffset != h.DataStart+8 {
		t.Fatalf("offsets byte=%d sample=%d, want byte=%d sample=2",
			fe.ByteOffset, fe.SampleOffset, h.DataStart+8)
	}
}

func TestDataTruncationReportsSample(t *testing.T) {
	raw := buildWAV(48000, 1, 16, make([]byte, 20))
	binary.LittleEndian.PutUint32(raw[40:44], 40) // data 声明 40，实际 20
	_, err := wav.DecodeHeader("w1", bytes.NewReader(raw))
	fe, ok := fault.As(err)
	if !ok || fe.ByteOffset != int64(len(raw)) {
		t.Fatalf("decode-time truncation = %v, want byte %d", err, len(raw))
	}

	// 头部合法但实际读出的 PCM 短于声明：CheckPCM 换算采样序号。
	good := buildWAV(48000, 1, 16, make([]byte, 40))
	h, err := wav.DecodeHeader("w1", bytes.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.CheckPCM("w1", good[h.DataStart:h.DataStart+20])
	fe, ok = fault.As(err)
	if !ok || fe.SampleOffset != 10 {
		t.Fatalf("check-time truncation = %v, want sample 10", err)
	}
}

func TestRejectNonPCMFormat(t *testing.T) {
	raw := buildWAV(48000, 1, 16, make([]byte, 4))
	binary.LittleEndian.PutUint16(raw[20:22], 3) // 非 PCM 格式标签
	_, err := wav.DecodeHeader("w1", bytes.NewReader(raw))
	if fe, ok := fault.As(err); !ok || fe.Kind != fault.KindMalformedInput {
		t.Fatalf("format err = %v", err)
	}
}
