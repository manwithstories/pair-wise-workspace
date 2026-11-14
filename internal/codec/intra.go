package codec

import (
	"errors"
	"fmt"
	"math"
)

// BitWriter appends bits MSB-first.
type BitWriter struct {
	buf   []byte
	nbits int
}

func (w *BitWriter) WriteBits(val uint64, n int) {
	for i := n - 1; i >= 0; i-- {
		bit := byte(val>>i) & 1
		bytePos := w.nbits / 8
		if bytePos == len(w.buf) {
			w.buf = append(w.buf, 0)
		}
		w.buf[bytePos] |= bit << (7 - uint(w.nbits%8))
		w.nbits++
	}
}

// WriteUE writes an unsigned exp-Golomb code.
func (w *BitWriter) WriteUE(v uint64) {
	v++
	bits := 0
	for t := v; t > 0; t >>= 1 {
		bits++
	}
	w.WriteBits(0, bits-1)
	w.WriteBits(v, bits)
}

// WriteSE writes a signed exp-Golomb code.
func (w *BitWriter) WriteSE(v int) {
	var u uint64
	if v > 0 {
		u = uint64(2*v - 1)
	} else {
		u = uint64(-2 * v)
	}
	w.WriteUE(u)
}

// Bits returns the number of bits written.
func (w *BitWriter) Bits() int { return w.nbits }

// Bytes returns the packed payload.
func (w *BitWriter) Bytes() []byte { return w.buf }

// BitReader reads bits MSB-first.
type BitReader struct {
	buf   []byte
	nbits int
	pos   int
}

func NewBitReader(buf []byte, nbits int) *BitReader {
	return &BitReader{buf: buf, nbits: nbits}
}

var ErrBitstreamEOF = errors.New("bitstream: unexpected end")

func (r *BitReader) ReadBits(n int) (uint64, error) {
	if r.pos+n > r.nbits {
		return 0, ErrBitstreamEOF
	}
	var v uint64
	for i := 0; i < n; i++ {
		bit := (r.buf[r.pos/8] >> (7 - uint(r.pos%8))) & 1
		v = v<<1 | uint64(bit)
		r.pos++
	}
	return v, nil
}

func (r *BitReader) ReadUE() (uint64, error) {
	zeros := 0
	for {
		b, err := r.ReadBits(1)
		if err != nil {
			return 0, err
		}
		if b == 1 {
			break
		}
		zeros++
		if zeros > 63 {
			return 0, errors.New("bitstream: exp-golomb overflow")
		}
	}
	if zeros == 0 {
		return 0, nil
	}
	suffix, err := r.ReadBits(zeros)
	if err != nil {
		return 0, err
	}
	return (uint64(1)<<zeros - 1) + suffix, nil
}

func (r *BitReader) ReadSE() (int, error) {
	u, err := r.ReadUE()
	if err != nil {
		return 0, err
	}
	if u%2 == 1 {
		return int((u + 1) / 2), nil
	}
	return -int(u / 2), nil
}

// extractBlock copies an 8x8 block from plane (stride w) with edge replication.
func extractBlock(plane []byte, w, h, bx, by int) *[64]float64 {
	out := &[64]float64{}
	for r := 0; r < BlockSize; r++ {
		sr := by + r
		if sr >= h {
			sr = h - 1
		}
		for c := 0; c < BlockSize; c++ {
			sc := bx + c
			if sc >= w {
				sc = w - 1
			}
			out[r*BlockSize+c] = float64(plane[sr*w+sc]) - 128
		}
	}
	return out
}

// storeBlock writes an 8x8 block back into plane, cropping to w,h.
func storeBlock(plane []byte, w, h, bx, by int, blk *[64]float64) {
	for r := 0; r < BlockSize; r++ {
		sr := by + r
		if sr >= h {
			break
		}
		for c := 0; c < BlockSize; c++ {
			sc := bx + c
			if sc >= w {
				break
			}
			v := int(math.Round(blk[r*BlockSize+c])) + 128
			if v < 0 {
				v = 0
			}
			if v > 255 {
				v = 255
			}
			plane[sr*w+sc] = byte(v)
		}
	}
}

func encodePlane(wr *BitWriter, plane []byte, w, h int, qtab *[64]int) {
	prevDC := 0
	for by := 0; by < h; by += BlockSize {
		for bx := 0; bx < w; bx += BlockSize {
			blk := extractBlock(plane, w, h, bx, by)
			coeffs := FDCT(blk)
			var zz [64]int
			for i := 0; i < 64; i++ {
				q := float64(qtab[i])
				zz[Zigzag[i]] = int(math.RoundToEven(coeffs[i] / q))
			}
			wr.WriteSE(zz[0] - prevDC)
			prevDC = zz[0]
			run := 0
			for i := 1; i < 64; i++ {
				if zz[i] == 0 {
					run++
					continue
				}
				wr.WriteUE(uint64(run) + 1)
				wr.WriteSE(zz[i])
				run = 0
			}
			wr.WriteUE(0) // end-of-block marker
		}
	}
}

func decodePlane(rd *BitReader, plane []byte, w, h int, qtab *[64]int) error {
	prevDC := 0
	for by := 0; by < h; by += BlockSize {
		for bx := 0; bx < w; bx += BlockSize {
			dcDiff, err := rd.ReadSE()
			if err != nil {
				return err
			}
			var zz [64]int
			zz[0] = prevDC + dcDiff
			prevDC = zz[0]
			pos := 1
			for {
				tok, err := rd.ReadUE()
				if err != nil {
					return err
				}
				if tok == 0 {
					break
				}
				run := tok - 1
				val, err := rd.ReadSE()
				if err != nil {
					return err
				}
				pos += int(run)
				if pos >= 64 {
					return errors.New("bitstream: coefficient overflow")
				}
				zz[pos] = val
				pos++
			}
			var block [64]float64
			for i := 0; i < 64; i++ {
				block[i] = float64(zz[Zigzag[i]] * qtab[i])
			}
			pixels := IDCT(&block)
			storeBlock(plane, w, h, bx, by, pixels)
		}
	}
	return nil
}

// EncodeFrame encodes one YUV420p frame (concatenated planes) at qp.
// Returns the payload and exact bit count.
func EncodeFrame(src []byte, w, h, qp int) ([]byte, int, error) {
	if qp < MinQP || qp > MaxQP {
		return nil, 0, fmt.Errorf("qp %d out of range [%d,%d]", qp, MinQP, MaxQP)
	}
	need := w * h * 3 / 2
	if len(src) < need {
		return nil, 0, fmt.Errorf("frame buffer too small: %d < %d", len(src), need)
	}
	y, u, v := src[:w*h], src[w*h:w*h+w*h/4], src[w*h+w*h/4:need]
	lq := ScaleQTable(LumaBase, qp)
	cq := ScaleQTable(ChromaBase, qp)
	wr := &BitWriter{}
	wr.WriteBits(uint64(qp), 8)
	encodePlane(wr, y, w, h, &lq)
	encodePlane(wr, u, w/2, h/2, &cq)
	encodePlane(wr, v, w/2, h/2, &cq)
	return wr.Bytes(), wr.Bits(), nil
}

// DecodeFrame decodes a payload produced by EncodeFrame.
func DecodeFrame(data []byte, bits, w, h int) ([]byte, int, error) {
	rd := NewBitReader(data, bits)
	qpv, err := rd.ReadBits(8)
	if err != nil {
		return nil, 0, err
	}
	qp := int(qpv)
	out := make([]byte, w*h*3/2)
	y, u, v := out[:w*h], out[w*h:w*h+w*h/4], out[w*h+w*h/4:]
	lq := ScaleQTable(LumaBase, qp)
	cq := ScaleQTable(ChromaBase, qp)
	if err := decodePlane(rd, y, w, h, &lq); err != nil {
		return nil, 0, err
	}
	if err := decodePlane(rd, u, w/2, h/2, &cq); err != nil {
		return nil, 0, err
	}
	if err := decodePlane(rd, v, w/2, h/2, &cq); err != nil {
		return nil, 0, err
	}
	return out, qp, nil
}
