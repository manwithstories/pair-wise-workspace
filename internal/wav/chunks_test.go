package wav

import (
	"testing"

	"pairflow/internal/clock"
	"pairflow/internal/testkit"
)

func TestParseTruncatedHeader(t *testing.T) {
	f := testkit.PCMFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 16}
	pcm := testkit.SynthesizePCM(f, 0, 8, 1)
	full := testkit.BuildWAVBlock(f, pcm)

	// 头截断：不足 RIFF/WAVE 最小长度，必须给出 truncated_header 结构化错误。
	trunc := testkit.TruncateWAV(full, 8)
	blk := ParseBlock("dev-x", 100, 1, 1, 1234, 0, trunc)
	if blk.Recoverable {
		t.Fatal("truncated header must be marked unrecoverable")
	}
	if len(blk.Errors) == 0 || blk.Errors[0].Kind != clock.KindTruncatedHeader {
		t.Fatalf("want truncated_header, got %+v", blk.Errors)
	}
	if blk.Errors[0].Offset != 100 {
		t.Fatalf("offset must be preserved, got %d", blk.Errors[0].Offset)
	}
}

func TestParseNonIntegralFrame(t *testing.T) {
	f := testkit.PCMFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 16}
	pcm := testkit.SynthesizePCM(f, 0, 8, 1)
	block := testkit.BuildWAVBlock(f, pcm)
	// 定位 data chunk 长度字段（chunkID 'data' 之后的 4 字节小端长度），
	// 改成比实际多 1 字节的奇数，制造非整帧（帧宽 4 字节）。
	dataLenOff := indexOfData(block)
	if dataLenOff < 0 {
		t.Fatal("data chunk not found")
	}
	block[dataLenOff+4] = byte(len(pcm) + 1)
	// 同时补足字节，使 chunk 体不被判定为截断。
	block = append(block, 0)
	blk := ParseBlock("dev-x", 0, 1, 1, 1, 0, block)
	found := false
	for _, e := range blk.Errors {
		if e.Kind == clock.KindNonIntegralFrame {
			found = true
		}
	}
	if !found {
		t.Fatal("non-integral frame must be reported")
	}
}

func indexOfData(block []byte) int {
	for i := 0; i+8 <= len(block); i++ {
		if string(block[i:i+4]) == "data" {
			return i
		}
	}
	return -1
}

func TestFormatChangeDetected(t *testing.T) {
	f1 := testkit.PCMFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 16}
	f2 := testkit.PCMFormat{SampleRate: 44100, Channels: 2, BitsPerSample: 16}
	b1 := ParseBlock("d", 0, 1, 1, 1, 0, testkit.BuildWAVBlock(f1, testkit.SynthesizePCM(f1, 0, 4, 1)))
	b2 := ParseBlock("d", 50, 2, 2, 2, 0, testkit.BuildWAVBlock(f2, testkit.SynthesizePCM(f2, 0, 4, 1)))
	se := DetectFormatChange(b1, b2)
	if se == nil || se.Kind != clock.KindFormatChanged {
		t.Fatalf("want format_changed, got %+v", se)
	}
	if se.Offset != 50 {
		t.Fatalf("offset must point at changed block, got %d", se.Offset)
	}
}

func TestParseHealthyBlock(t *testing.T) {
	f := testkit.PCMFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 16}
	pcm := testkit.SynthesizePCM(f, 0, 12, 1)
	blk := ParseBlock("d", 7, 3, 3, 9, 0, testkit.BuildWAVBlock(f, pcm))
	if len(blk.Errors) != 0 {
		t.Fatalf("healthy block must have no errors, got %+v", blk.Errors)
	}
	if blk.SampleCount() != 12 || blk.DataOffset <= 7 {
		t.Fatalf("bad parse: frames=%d dataOffset=%d", blk.SampleCount(), blk.DataOffset)
	}
}
