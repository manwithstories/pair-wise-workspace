package codec

import (
	"bytes"
	"fmt"

	"turncode/internal/yuv"
)

const (
	motionRadius = 4
	frameI       = false
	frameP       = true
)

// Encoder maintains the reconstructed reference frame across a GOP. It is not
// safe for concurrent use; the pipeline serializes GOPs within each job.
type Encoder struct {
	width  int
	height int
	ref    *yuv.Frame
}

func NewEncoder(width, height int) (*Encoder, error) {
	if width <= 0 || height <= 0 || width%2 != 0 || height%2 != 0 {
		return nil, fmt.Errorf("codec: invalid dimensions %dx%d", width, height)
	}
	return &Encoder{width: width, height: height}, nil
}

func (e *Encoder) Reset()                { e.ref = nil }
func (e *Encoder) Reference() *yuv.Frame { return e.ref }

// EncodeFrame encodes one source frame at the given QP, updates the reference,
// and returns a self-contained payload plus its reconstructed frame.
func (e *Encoder) EncodeFrame(src *yuv.Frame, isP bool, qp int) ([]byte, *yuv.Frame, error) {
	if err := src.Validate(); err != nil {
		return nil, nil, err
	}
	if src.Width != e.width || src.Height != e.height {
		return nil, nil, fmt.Errorf("codec: source dimensions do not match encoder")
	}
	if isP && e.ref == nil {
		return nil, nil, fmt.Errorf("codec: P frame without reference")
	}
	qp = clampQP(qp)
	recon, err := yuv.NewFrame(e.width, e.height)
	if err != nil {
		return nil, nil, err
	}
	var out bytes.Buffer
	bw := NewBitWriter(&out)
	bw.WriteBit(isP)
	bw.WriteBits(uint64(qp), 6)

	lumaBW, lumaBH := blockDims(e.width, e.height)
	chromaBW, chromaBH := blockDims(e.width/2, e.height/2)
	mbCols, mbRows := (e.width+15)/16, (e.height+15)/16
	mv := make([][2]int, mbCols*mbRows)
	if isP {
		for by := 0; by < mbRows; by++ {
			for bx := 0; bx < mbCols; bx++ {
				idx := by*mbCols + bx
				if bx*16 >= e.width || by*16 >= e.height {
					bw.WriteBit(false)
					continue
				}
				dx, dy := searchMotion(src.Y, e.ref.Y, e.width, e.height, bx*16, by*16, motionRadius)
				mv[idx] = [2]int{dx, dy}
				if dx == 0 && dy == 0 {
					bw.WriteBit(false)
				} else {
					bw.WriteBit(true)
					bw.WriteSE(dx)
					bw.WriteSE(dy)
				}
			}
		}
	}

	for by := 0; by < lumaBH; by++ {
		for bx := 0; bx < lumaBW; bx++ {
			pred := predictor(e.ref, 0, bx, by, 8, mv, mbCols, isP)
			block := sampleBlock(src.Y, e.width, e.height, bx*8, by*8, pred)
			coeff := encodeBlock(bw, block, qp)
			writeReconBlock(recon.Y, e.width, e.height, bx*8, by*8, coeff, qp, pred)
		}
	}
	writeChromaBlocks(bw, src.U, recon.U, e.width/2, e.height/2, chromaBW, chromaBH, e.ref, 1, qp, mv, mbCols)
	writeChromaBlocks(bw, src.V, recon.V, e.width/2, e.height/2, chromaBW, chromaBH, e.ref, 2, qp, mv, mbCols)
	if err := bw.Flush(); err != nil {
		return nil, nil, err
	}
	e.ref = recon
	return out.Bytes(), recon, nil
}

func writeChromaBlocks(bw *BitWriter, src, dst []byte, width, height, bwCount, bhCount int, ref *yuv.Frame, plane int, qp int, mv [][2]int, mbCols int) {
	for by := 0; by < bhCount; by++ {
		for bx := 0; bx < bwCount; bx++ {
			pred := dcPrediction()
			if ref != nil {
				idx := by*mbCols + bx
				dx, dy := 0, 0
				if idx < len(mv) {
					dx, dy = mv[idx][0]/2, mv[idx][1]/2
				}
				var planeData []byte
				switch plane {
				case 1:
					planeData = ref.U
				case 2:
					planeData = ref.V
				}
				pred = predictPlane(planeData, width, height, bx*8, by*8, dx, dy)
			}
			block := sampleBlock(src, width, height, bx*8, by*8, pred)
			coeff := encodeBlock(bw, block, qp)
			writeReconBlock(dst, width, height, bx*8, by*8, coeff, qp, pred)
		}
	}
}

func encodeBlock(bw *BitWriter, samples Block, qp int) QuantBlock {
	coeff := Quantize(ForwardDCT(samples), qp)
	scan := Zigzag(coeff)
	acCount := 0
	for i := 1; i < BlockPixels; i++ {
		if scan[i] != 0 {
			acCount++
		}
	}
	bw.WriteBit(acCount != 0)
	bw.WriteSE(scan[0])
	if acCount == 0 {
		return coeff
	}
	bw.WriteUE(uint64(acCount))
	run := 0
	for i := 1; i < BlockPixels; i++ {
		if scan[i] == 0 {
			run++
			continue
		}
		bw.WriteUE(uint64(run))
		bw.WriteSE(scan[i])
		run = 0
	}
	return coeff
}

func sampleBlock(plane []byte, width, height, x0, y0 int, pred Block) Block {
	var out Block
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			sx, sy := x0+x, y0+y
			if sx >= width {
				sx = width - 1
			}
			if sy >= height {
				sy = height - 1
			}
			out[y*8+x] = float64(plane[sy*width+sx]) - pred[y*8+x]
		}
	}
	return out
}

func writeReconBlock(dst []byte, width, height, x0, y0 int, coeff QuantBlock, qp int, pred Block) {
	rec := InverseDCT(Dequantize(coeff, qp))
	for y := 0; y < 8; y++ {
		dy := y0 + y
		if dy >= height {
			break
		}
		for x := 0; x < 8; x++ {
			dx := x0 + x
			if dx >= width {
				break
			}
			dst[dy*width+dx] = clampByte(pred[y*8+x] + rec[y*8+x])
		}
	}
}

func predictor(ref *yuv.Frame, plane, bx, by, blockSize int, mv [][2]int, mbCols int, isP bool) Block {
	if !isP || ref == nil {
		return dcPrediction()
	}
	idx := (by/2)*mbCols + bx/2
	dx, dy := 0, 0
	if idx < len(mv) {
		dx, dy = mv[idx][0], mv[idx][1]
	}
	planeData := ref.Y
	if plane == 1 {
		planeData = ref.U
	} else if plane == 2 {
		planeData = ref.V
	}
	_ = blockSize
	return predictPlane(planeData, ref.Width, ref.Height, bx*8, by*8, dx, dy)
}

func dcPrediction() Block {
	var dc Block
	for i := range dc {
		dc[i] = 128
	}
	return dc
}

func predictPlane(plane []byte, width, height, x0, y0, dx, dy int) Block {
	var out Block
	for y := 0; y < 8; y++ {
		sy := y0 + y + dy
		if sy < 0 {
			sy = 0
		} else if sy >= height {
			sy = height - 1
		}
		for x := 0; x < 8; x++ {
			sx := x0 + x + dx
			if sx < 0 {
				sx = 0
			} else if sx >= width {
				sx = width - 1
			}
			out[y*8+x] = float64(plane[sy*width+sx])
		}
	}
	return out
}

func blockDims(width, height int) (int, int) {
	return (width + 7) / 8, (height + 7) / 8
}

func searchMotion(src, ref []byte, width, height, x0, y0, radius int) (int, int) {
	zeroSad := sad(src, ref, width, height, x0, y0, 0, 0)
	if zeroSad == 0 {
		return 0, 0
	}
	bestX, bestY, bestSad := 0, 0, zeroSad
	for dy := -radius; dy <= radius; dy += 2 {
		for dx := -radius; dx <= radius; dx += 2 {
			if dx == 0 && dy == 0 {
				continue
			}
			s := sad(src, ref, width, height, x0, y0, dx, dy)
			if s < bestSad {
				bestX, bestY, bestSad = dx, dy, s
			}
		}
	}
	return bestX, bestY
}

func sad(src, ref []byte, width, height, x0, y0, dx, dy int) int {
	sum := 0
	for y := 0; y < 16; y += 2 {
		sy := y0 + y
		if sy >= height {
			break
		}
		ry := sy + dy
		if ry < 0 {
			ry = 0
		} else if ry >= height {
			ry = height - 1
		}
		for x := 0; x < 16; x += 2 {
			sx := x0 + x
			if sx >= width {
				break
			}
			rx := sx + dx
			if rx < 0 {
				rx = 0
			} else if rx >= width {
				rx = width - 1
			}
			d := int(src[sy*width+sx]) - int(ref[ry*width+rx])
			if d < 0 {
				d = -d
			}
			sum += d
		}
	}
	return sum
}

// DecodeFrame decodes one payload and returns the reconstructed frame, QP and
// frame type. P frames require the previous reconstructed frame as reference.
func DecodeFrame(payload []byte, width, height int, ref *yuv.Frame) (*yuv.Frame, int, bool, error) {
	if width <= 0 || height <= 0 || width%2 != 0 || height%2 != 0 {
		return nil, 0, false, fmt.Errorf("codec: invalid dimensions %dx%d", width, height)
	}
	br := NewBitReader(payload)
	isP := br.ReadBit()
	qp := int(br.ReadBits(6))
	if br.Err() != nil || qp < 1 || qp > 51 {
		return nil, 0, false, fmt.Errorf("%w: frame header", ErrBitstream)
	}
	if isP && ref == nil {
		return nil, 0, false, fmt.Errorf("codec: P frame without reference")
	}
	if ref != nil && (ref.Width != width || ref.Height != height) {
		return nil, 0, false, fmt.Errorf("codec: reference dimensions")
	}
	recon, err := yuv.NewFrame(width, height)
	if err != nil {
		return nil, 0, false, err
	}
	mbCols, mbRows := (width+15)/16, (height+15)/16
	mv := make([][2]int, mbCols*mbRows)
	if isP {
		for by := 0; by < mbRows; by++ {
			for bx := 0; bx < mbCols; bx++ {
				if br.ReadBit() {
					dx, dy := br.ReadSE(), br.ReadSE()
					if dx < -64 || dx > 64 || dy < -64 || dy > 64 {
						return nil, 0, false, fmt.Errorf("%w: motion vector", ErrBitstream)
					}
					mv[by*mbCols+bx] = [2]int{dx, dy}
				}
			}
		}
	}
	lumaBW, lumaBH := blockDims(width, height)
	for by := 0; by < lumaBH; by++ {
		for bx := 0; bx < lumaBW; bx++ {
			coeff, err := decodeBlock(br)
			if err != nil {
				return nil, 0, false, err
			}
			pred := predictor(ref, 0, bx, by, 8, mv, mbCols, isP)
			writeReconBlock(recon.Y, width, height, bx*8, by*8, coeff, qp, pred)
		}
	}
	chromaBW, chromaBH := blockDims(width/2, height/2)
	if err := decodeChroma(br, recon.U, width/2, height/2, chromaBW, chromaBH, ref, 1, qp, mv, mbCols); err != nil {
		return nil, 0, false, err
	}
	if err := decodeChroma(br, recon.V, width/2, height/2, chromaBW, chromaBH, ref, 2, qp, mv, mbCols); err != nil {
		return nil, 0, false, err
	}
	if br.Err() != nil {
		return nil, 0, false, br.Err()
	}
	return recon, qp, isP, nil
}

func decodeChroma(br *BitReader, dst []byte, width, height, bwCount, bhCount int, ref *yuv.Frame, plane, qp int, mv [][2]int, mbCols int) error {
	for by := 0; by < bhCount; by++ {
		for bx := 0; bx < bwCount; bx++ {
			coeff, err := decodeBlock(br)
			if err != nil {
				return err
			}
			pred := dcPrediction()
			if ref != nil {
				dx, dy := 0, 0
				idx := by*mbCols + bx
				if idx < len(mv) {
					dx, dy = mv[idx][0]/2, mv[idx][1]/2
				}
				planeData := ref.U
				if plane == 2 {
					planeData = ref.V
				}
				pred = predictPlane(planeData, width, height, bx*8, by*8, dx, dy)
			}
			writeReconBlock(dst, width, height, bx*8, by*8, coeff, qp, pred)
		}
	}
	return nil
}

func decodeBlock(br *BitReader) (QuantBlock, error) {
	hasAC := br.ReadBit()
	dc := br.ReadSE()
	if dc < -32767 || dc > 32767 {
		return QuantBlock{}, fmt.Errorf("%w: dc coefficient", ErrBitstream)
	}
	var scan [BlockPixels]int
	scan[0] = dc
	if hasAC {
		count := br.ReadUE()
		if count == 0 || count > 63 {
			return QuantBlock{}, fmt.Errorf("%w: ac count %d", ErrBitstream, count)
		}
		pos := 0
		for i := uint64(0); i < count; i++ {
			run := br.ReadUE()
			level := br.ReadSE()
			if run > 63 || pos+int(run)+1 >= BlockPixels || level < -32767 || level > 32767 {
				return QuantBlock{}, fmt.Errorf("%w: ac coefficient", ErrBitstream)
			}
			pos += int(run) + 1
			scan[pos] = level
		}
	}
	if br.Err() != nil {
		return QuantBlock{}, br.Err()
	}
	return Unzigzag(scan), nil
}
