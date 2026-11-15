package codec

import (
	"encoding/binary"
	"fmt"
	"math"

	"rcctl/internal/source"
)

// FrameResult contains the encoded size and the decoder-side reconstruction.
type FrameResult struct {
	Bytes         int
	Data          []byte
	Reconstructed source.Frame
	Q             float64
	Intra         bool
	SkippedBlocks int
	EncodedBlocks int
}

// Encoder is stateless; the caller supplies the reference reconstruction.
type Encoder struct {
	Width  int
	Height int
}

// NewEncoder validates frame dimensions.
func NewEncoder(width, height int) (*Encoder, error) {
	if width < 16 || height < 16 || width%16 != 0 || height%16 != 0 {
		return nil, fmt.Errorf("encoder dimensions must be multiples of 16")
	}
	return &Encoder{Width: width, Height: height}, nil
}

// EncodeFrame applies DCT quantization to intra or inter residuals.
func (e *Encoder) EncodeFrame(src source.Frame, reference *source.Frame, q float64, intra bool) (FrameResult, error) {
	if src.Width != e.Width || src.Height != e.Height {
		return FrameResult{}, fmt.Errorf("frame is %dx%d, encoder is %dx%d", src.Width, src.Height, e.Width, e.Height)
	}
	if !intra && reference == nil {
		intra = true
	}
	if q < 0.5 {
		q = 0.5
	}
	if q > 255 {
		q = 255
	}
	res := FrameResult{Q: q, Intra: intra, Reconstructed: source.Frame{
		Index: src.Index, Width: src.Width, Height: src.Height,
		Y: make([]byte, len(src.Y)), U: make([]byte, len(src.U)), V: make([]byte, len(src.V)),
	}}
	res.Data = make([]byte, 0, 8192)
	res.Data = append(res.Data, 'R', 'C', 1)
	var head [8]byte
	binary.LittleEndian.PutUint32(head[:4], uint32(src.Index))
	binary.LittleEndian.PutUint16(head[4:6], uint16(q*16))
	if intra {
		head[6] = 1
	}
	res.Data = append(res.Data, head[:7]...)

	e.encodePlane(src.Y, referenceY(reference), res.Reconstructed.Y, e.Width, e.Height, q, intra, &res)
	e.encodePlane(src.U, referenceU(reference), res.Reconstructed.U, e.Width/2, e.Height/2, q, intra, &res)
	e.encodePlane(src.V, referenceV(reference), res.Reconstructed.V, e.Width/2, e.Height/2, q, intra, &res)
	res.Bytes = len(res.Data)
	return res, nil
}

func referenceY(f *source.Frame) []byte {
	if f == nil {
		return nil
	}
	return f.Y
}

func referenceU(f *source.Frame) []byte {
	if f == nil {
		return nil
	}
	return f.U
}

func referenceV(f *source.Frame) []byte {
	if f == nil {
		return nil
	}
	return f.V
}

func (e *Encoder) encodePlane(src, reference, dst []byte, width, height int, q float64, intra bool, res *FrameResult) {
	for by := 0; by < height; by += BlockSize {
		for bx := 0; bx < width; bx += BlockSize {
			var residual, reconstructed [BlockSize * BlockSize]float64
			for y := 0; y < BlockSize; y++ {
				for x := 0; x < BlockSize; x++ {
					i := (by+y)*width + bx + x
					pred := 128.0
					if !intra && reference != nil {
						pred = float64(reference[i])
					}
					residual[y*BlockSize+x] = float64(src[i]) - pred
					reconstructed[y*BlockSize+x] = pred
				}
			}
			coeff := Forward(&residual)
			quantized := Quantize(&coeff, q)
			if allZero(&quantized) {
				res.SkippedBlocks++
				res.Data = append(res.Data, 0)
			} else {
				res.EncodedBlocks++
				res.Data = append(res.Data, 1)
				writeBlock(res, &quantized)
			}
			restored := Inverse(ptr(Dequantize(&quantized, q)))
			for y := 0; y < BlockSize; y++ {
				for x := 0; x < BlockSize; x++ {
					i := (by+y)*width + bx + x
					dst[i] = clamp8(reconstructed[y*BlockSize+x] + restored[y*BlockSize+x])
				}
			}
		}
	}
}

func writeBlock(res *FrameResult, q *[BlockSize * BlockSize]int32) {
	var zig [BlockSize * BlockSize]int
	last := -1
	for n, idx := range Zigzag {
		zig[n] = int(q[idx])
		if zig[n] != 0 {
			last = n
		}
	}
	if last < 0 {
		return
	}
	res.Data = append(res.Data, byte(last))
	run := 0
	for n := 0; n <= last; n++ {
		if zig[n] == 0 {
			run++
			continue
		}
		res.Data = appendUvarint(res.Data, uint64(run))
		res.Data = appendUvarint(res.Data, zigzagValue(zig[n]))
		run = 0
	}
}

func appendUvarint(dst []byte, v uint64) []byte {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], v)
	return append(dst, buf[:n]...)
}

func zigzagValue(v int) uint64 {
	return uint64(uint32(v<<1) ^ uint32(v>>31))
}

func allZero(q *[BlockSize * BlockSize]int32) bool {
	for _, v := range q {
		if v != 0 {
			return false
		}
	}
	return true
}

func ptr(v [BlockSize * BlockSize]float64) *[BlockSize * BlockSize]float64 {
	return &v
}

func clamp8(v float64) byte {
	if math.IsNaN(v) || v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return byte(math.Round(v))
}
