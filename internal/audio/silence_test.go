package audio

import (
	"encoding/binary"
	"testing"
)

func pcmBytes(vals ...int16) []byte {
	b := make([]byte, 2*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

func TestRMSAndPCM16(t *testing.T) {
	pcm, odd := PCM16(pcmBytes(0, 0, 0, 0))
	if odd || RMS(pcm) != 0 {
		t.Fatalf("zero PCM RMS = %v odd=%v", RMS(pcm), odd)
	}
	loud := []float64{0.5, -0.5, 0.5, -0.5}
	if r := RMS(loud); r < 0.49 || r > 0.51 {
		t.Fatalf("RMS = %v, want ~0.5", r)
	}
	// 奇数字节：最后一个字节忽略并标记。
	_, odd = PCM16(append(pcmBytes(1, 2), 0xFF))
	if !odd {
		t.Fatal("odd byte count must be flagged")
	}
}

func TestDetectSilentAndLoudWindows(t *testing.T) {
	segs := []SegmentPCM{
		{SegmentID: "s1", Data: pcmBytes(0, 0, 0, 0), Complete: true},
		{SegmentID: "s2", Data: pcmBytes(16000, -16000, 16000, -16000), Complete: true},
	}
	ws := DetectWindows("trk-1", 0, 1, 4, segs)
	if len(ws) != 2 {
		t.Fatalf("windows = %d, want 2", len(ws))
	}
	if !ws[0].Silent || ws[0].Incomplete {
		t.Fatalf("first window must be fully silent: %+v", ws[0])
	}
	if ws[1].Silent {
		t.Fatalf("loud window must not be silent: RMS=%v", ws[1].RMS)
	}
	ranges := SilenceRanges(ws)
	if len(ranges) != 1 || ranges[0] != [2]int64{0, 4} {
		t.Fatalf("silence ranges = %v", ranges)
	}
}

func TestWindowSpansSegmentBoundary(t *testing.T) {
	// 窗口大小 4，但每个片段只有 2 个样本：窗口必须跨片段拼接。
	segs := []SegmentPCM{
		{SegmentID: "s1", Data: pcmBytes(0, 0), Complete: true},
		{SegmentID: "s2", Data: pcmBytes(0, 0), Complete: true},
		{SegmentID: "s3", Data: pcmBytes(0, 0), Complete: true},
		{SegmentID: "s4", Data: pcmBytes(0, 0), Complete: true},
	}
	ws := DetectWindows("trk-1", 100, 10, 4, segs)
	if len(ws) != 2 {
		t.Fatalf("cross-segment windows = %d, want 2", len(ws))
	}
	if ws[0].StartTick != 100 || ws[0].EndTick != 140 {
		t.Fatalf("window tick span wrong: %+v", ws[0])
	}
	for _, w := range ws {
		if !w.Silent {
			t.Fatalf("all-zero window must be silent: %+v", w)
		}
	}
}

func TestTruncatedWindowNotSilent(t *testing.T) {
	segs := []SegmentPCM{
		{SegmentID: "s1", Data: pcmBytes(0, 0, 0, 0), Complete: true},
		{SegmentID: "s2", Data: pcmBytes(0, 0, 0, 0), Complete: false},
	}
	ws := DetectWindows("trk-1", 0, 1, 4, segs)
	if len(ws) != 2 {
		t.Fatalf("windows = %d", len(ws))
	}
	if !ws[0].Silent {
		t.Fatal("complete window must be silent")
	}
	// 截断窗口即便全零也不得判静音。
	if !ws[1].Incomplete || ws[1].Silent {
		t.Fatalf("truncated window misreported: %+v", ws[1])
	}
	if ranges := SilenceRanges(ws); len(ranges) != 1 || ranges[0][1] != 4 {
		t.Fatalf("truncation must end silence range: %v", ranges)
	}
}

func TestOddByteWindowIncomplete(t *testing.T) {
	segs := []SegmentPCM{{
		SegmentID: "s1",
		Data:      append(pcmBytes(0, 0, 0, 0), 0xAB),
		Complete:  true, // 声明完整但字节奇数，解码器仍须识别
	}}
	ws := DetectWindows("trk-1", 0, 1, 4, segs)
	if len(ws) != 1 || !ws[0].Incomplete || ws[0].Silent {
		t.Fatalf("odd-byte window wrong: %+v", ws)
	}
}
