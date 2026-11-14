package codec

import (
	"math"

	"rcx/internal/fixture"
)

// QPMin / QPMax 限定量化参数范围（浮点，0.25 步进）。
const (
	QPMin = 0.25
	QPMax = 51.0
)

// ClampQP 把 QP 限制在合法区间。
func ClampQP(qp float64) float64 {
	if qp < QPMin {
		return QPMin
	}
	if qp > QPMax {
		return QPMax
	}
	return qp
}

// encodePlane 编码并重建一个平面，返回熵编码比特数。
func encodePlane(src, dst []byte, w, h int, qp float64, qm *[64]float64) int64 {
	var in, out [64]float64
	var q [64]int
	var total int64
	qs := qscale(qp)
	for by := 0; by+8 <= h; by += 8 {
		for bx := 0; bx+8 <= w; bx += 8 {
			for y := 0; y < 8; y++ {
				row := (by+y)*w + bx
				for x := 0; x < 8; x++ {
					in[y*8+x] = float64(src[row+x]) - 128
				}
			}
			fdct8x8(&in, &out)
			for i := 0; i < 64; i++ {
				step := qm[i] * qs
				q[i] = int(math.Round(out[i] / step))
			}
			total += blockBits(&q)
			for i := 0; i < 64; i++ {
				out[i] = float64(q[i]) * qm[i] * qs
			}
			idct8x8(&out, &in)
			for y := 0; y < 8; y++ {
				row := (by+y)*w + bx
				for x := 0; x < 8; x++ {
					v := in[y*8+x] + 128
					if v < 0 {
						v = 0
					} else if v > 255 {
						v = 255
					}
					dst[row+x] = byte(v + 0.5)
				}
			}
		}
	}
	return total
}

// EncodeFrame 帧内编码一帧，返回比特数与重建帧。
// 色度使用略粗的量化（qp+4）。
func EncodeFrame(src *fixture.Frame, qp float64) (int64, *fixture.Frame) {
	qp = ClampQP(qp)
	recon := fixture.NewFrame(src.W, src.H)
	bits := encodePlane(src.Y, recon.Y, src.W, src.H, qp, &lumQ)
	bits += encodePlane(src.U, recon.U, src.W/2, src.H/2, qp+4, &chrQ)
	bits += encodePlane(src.V, recon.V, src.W/2, src.H/2, qp+4, &chrQ)
	return bits, recon
}

// FrameBits 只统计一帧在给定 QP 下的比特数，不保留重建帧。
func FrameBits(src *fixture.Frame, qp float64) int64 {
	bits, recon := EncodeFrame(src, qp)
	_ = recon
	return bits
}

// SegResult 是一段编码的结果。
type SegResult struct {
	Bits      int64
	FrameBits []int64
	Recon     []*fixture.Frame
}

// EncodeSegment 以固定 QP 编码一段帧，返回真实比特与重建帧。
func EncodeSegment(frames []*fixture.Frame, qp float64, keepRecon bool) *SegResult {
	res := &SegResult{FrameBits: make([]int64, len(frames))}
	if keepRecon {
		res.Recon = make([]*fixture.Frame, len(frames))
	}
	for i, f := range frames {
		bits, recon := EncodeFrame(f, qp)
		res.FrameBits[i] = bits
		res.Bits += bits
		if keepRecon {
			res.Recon[i] = recon
		}
	}
	return res
}
