package audio

import (
	"math"
	"unsafe"

	"github.com/asticode/go-astiav"
)

// filterKind 区分双二阶滤波器类型。
type filterKind int

const (
	highPass filterKind = iota
)

// makeRBQ 按 Robert Bristow-Johnson 公式构造双二阶节。
// w0 为归一化角频率（f/fs），q 为品质因数。
func makeRBQ(kind filterKind, w0Norm, q float64) biquad {
	w0 := 2 * math.Pi * w0Norm
	cosw := math.Cos(w0)
	sinw := math.Sin(w0)
	alpha := sinw / (2 * q)
	var b biquad
	switch kind {
	case highPass:
		b0 := (1+cosw)/2
		b1 := -(1 + cosw)
		b2 := (1 + cosw) / 2
		a0 := 1 + alpha
		a1 := -2 * cosw
		a2 := 1 - alpha
		b = biquad{b0 / a0, b1 / a0, b2 / a0, a1 / a0, a2 / a0, 0, 0, 0, 0}
	}
	return b
}

// makeHighShelf 构造高频架滤波器（RBJ cookbook），dB 为提升量。
func makeHighShelf(w0Norm, gainDB float64) biquad {
	w0 := 2 * math.Pi * w0Norm
	cosw := math.Cos(w0)
	sinw := math.Sin(w0)
	a := math.Pow(10, gainDB/40)
	sqrt2a := 2 * math.Sqrt(a) * sinw
	b0 := a * ((a + 1) + (a-1)*cosw + sqrt2a)
	b1 := -2 * a * ((a - 1) + (a+1)*cosw)
	b2 := a * ((a + 1) + (a-1)*cosw - sqrt2a)
	a0 := (a + 1) - (a-1)*cosw + sqrt2a
	a1 := 2 * ((a - 1) - (a+1)*cosw)
	a2 := (a + 1) - (a-1)*cosw - sqrt2a
	return biquad{b0 / a0, b1 / a0, b2 / a0, a1 / a0, a2 / a0, 0, 0, 0, 0}
}

// planeBytes 取出 planar 帧指定声道的 float32 平面，长度恰为 NbSamples*4。
// Bytes() 经 av_samples_copy_to_buffer 产出紧密排列缓冲，按平面实际大小切分。
func planeBytes(f *astiav.Frame, ch int) []byte {
	data, err := f.Data().Bytes(0)
	if err != nil {
		return nil
	}
	need := f.NbSamples() * 4
	start := ch * need
	if start < 0 || start+need > len(data) {
		return nil
	}
	return data[start : start+need]
}

// bytesToF32 把原始字节零拷贝解释为 float32 切片。
func bytesToF32(b []byte) []float32 {
	if len(b) < 4 {
		return nil
	}
	n := len(b) / 4
	return unsafe.Slice((*float32)(unsafe.Pointer(&b[0])), n)
}
