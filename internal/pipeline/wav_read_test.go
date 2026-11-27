package pipeline

import (
	"encoding/binary"
	"math"
	"os"
	"testing"
)

// readWAVMono 读取 16-bit 立体声 WAV，下混为单声道 float32。
func readWAVMono(t *testing.T, path string) []float32 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < wavHeaderLen {
		t.Fatal("WAV 过短")
	}
	pcm := data[wavHeaderLen:]
	if len(pcm)%4 != 0 {
		t.Fatal("立体声 16-bit PCM 长度应为 4 的倍数")
	}
	frames := len(pcm) / 4
	mono := make([]float32, frames)
	for i := 0; i < frames; i++ {
		l := int16(binary.LittleEndian.Uint16(pcm[i*4:]))
		rr := int16(binary.LittleEndian.Uint16(pcm[i*4+2:]))
		mono[i] = float32(float64(l)+float64(rr)) * 0.5 / 32767.0
	}
	return mono
}

// stereoFromMono 把单声道样本复制成交错立体声。
func stereoFromMono(mono []float32) []float32 {
	out := make([]float32, len(mono)*2)
	for i, v := range mono {
		out[i*2] = v
		out[i*2+1] = v
	}
	return out
}

var _ = math.Pi
