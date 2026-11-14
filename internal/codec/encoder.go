package codec

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
)

// EncodeFrame performs an intra-only transform/quantize/entropy encode.
func EncodeFrame(src []byte, width, height, qp int) ([]byte, error) {
	if width <= 0 || height <= 0 || width*height != len(src) {
		return nil, fmt.Errorf("invalid frame %dx%d with %d bytes", width, height, len(src))
	}
	if qp < 0 || qp > 63 {
		return nil, fmt.Errorf("qp out of range: %d", qp)
	}
	var bw BitWriter
	for by := 0; by < height; by += blockSide {
		for bx := 0; bx < width; bx += blockSide {
			var block, coeff [64]float64
			for y := 0; y < blockSide; y++ {
				sy := by + y
				if sy >= height {
					sy = height - 1
				}
				for x := 0; x < blockSide; x++ {
					sx := bx + x
					if sx >= width {
						sx = width - 1
					}
					block[y*blockSide+x] = float64(src[sy*width+sx]) - 128
				}
			}
			ForwardDCT(&block, &coeff)
			q := QuantizeZigzag(&coeff, qp)
			allZero := true
			for _, value := range q {
				if value != 0 {
					allZero = false
					break
				}
			}
			if allZero {
				bw.WriteBit(0)
				continue
			}
			bw.WriteBit(1)
			bw.WriteSE(q[0])
			run := uint64(0)
			for k := 1; k < 64; k++ {
				if q[k] == 0 {
					run++
					continue
				}
				bw.WriteUE(run)
				bw.WriteSE(q[k])
				run = 0
			}
			bw.WriteUE(64)
		}
	}
	return bw.Bytes(), nil
}

// DecodeFrame reconstructs one frame from an intra payload.
func DecodeFrame(payload []byte, width, height, qp int) ([]byte, error) {
	if width <= 0 || height <= 0 || qp < 0 || qp > 63 {
		return nil, fmt.Errorf("%w: invalid frame parameters", ErrCorrupt)
	}
	out := make([]byte, width*height)
	br := NewBitReader(payload)
	for by := 0; by < height; by += blockSide {
		for bx := 0; bx < width; bx += blockSide {
			skip, err := br.ReadBit()
			if err != nil {
				return nil, err
			}
			if skip == 0 {
				for y := 0; y < blockSide; y++ {
					dy := by + y
					if dy >= height {
						continue
					}
					for x := 0; x < blockSide; x++ {
						dx := bx + x
						if dx < width {
							out[dy*width+dx] = 128
						}
					}
				}
				continue
			}
			var q [64]int32
			dc, err := br.ReadSE()
			if err != nil {
				return nil, err
			}
			q[0] = dc
			k := 1
			for {
				run, err := br.ReadUE()
				if err != nil {
					return nil, err
				}
				if run == 64 {
					break
				}
				k += int(run)
				if k < 1 || k >= 64 {
					return nil, fmt.Errorf("%w: invalid coefficient run", ErrCorrupt)
				}
				value, err := br.ReadSE()
				if err != nil {
					return nil, err
				}
				q[k] = value
				k++
			}
			coeff := DequantizeZigzag(q, qp)
			var block [64]float64
			InverseDCT(&coeff, &block)
			for y := 0; y < blockSide; y++ {
				dy := by + y
				if dy >= height {
					continue
				}
				for x := 0; x < blockSide; x++ {
					dx := bx + x
					if dx >= width {
						continue
					}
					out[dy*width+dx] = clampPixel(block[y*blockSide+x] + 128)
				}
			}
		}
	}
	return out, nil
}

func clampPixel(v float64) byte {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return byte(math.Round(v))
}

// SegmentMeta describes an independently addressable segment file.
type SegmentMeta struct {
	ID     int
	QP     int
	Width  int
	Height int
	FPS    int
	Frames int
}

type SegmentData struct {
	Bytes       []byte
	Meta        SegmentMeta
	Bits        uint64
	PSNR        float64
	CRC         uint32
	FrameHashes [][32]byte
}

// EncodeSegment encodes every frame, decodes it back to derive a frame hash
// and PSNR, and wraps the payload in a length/CRC protected segment file.
func EncodeSegment(meta SegmentMeta, frames [][]byte) (*SegmentData, error) {
	if meta.ID <= 0 || meta.ID > 65535 || meta.FPS <= 0 || meta.Frames <= 0 || meta.Frames > 65535 {
		return nil, fmt.Errorf("invalid segment metadata")
	}
	if len(frames) != meta.Frames {
		return nil, fmt.Errorf("segment %d: got %d frames, want %d", meta.ID, len(frames), meta.Frames)
	}
	payload := make([]byte, 0, meta.Frames*1024)
	hashes := make([][32]byte, 0, meta.Frames)
	psnrSum := 0.0
	for i, frame := range frames {
		payloadBytes, err := EncodeFrame(frame, meta.Width, meta.Height, meta.QP)
		if err != nil {
			return nil, fmt.Errorf("segment %d frame %d encode: %w", meta.ID, i, err)
		}
		recon, err := DecodeFrame(payloadBytes, meta.Width, meta.Height, meta.QP)
		if err != nil {
			return nil, fmt.Errorf("segment %d frame %d decode: %w", meta.ID, i, err)
		}
		h := sha256.Sum256(recon)
		hashes = append(hashes, h)
		psnrSum += framePSNR(frame, recon)
		var fm [FrameMetaSize]byte
		putFrameMeta(fm[:], frameMeta{index: uint16(i), payloadLen: uint32(len(payloadBytes)), payloadCRC: crc32.ChecksumIEEE(payloadBytes), decodedHash: h})
		payload = append(payload, fm[:]...)
		payload = append(payload, payloadBytes...)
	}
	header := SegmentHeader{
		Version:    SegmentVersion,
		ID:         uint16(meta.ID),
		QP:         uint16(meta.QP),
		Width:      uint16(meta.Width),
		Height:     uint16(meta.Height),
		FPS:        uint16(meta.FPS),
		Frames:     uint16(meta.Frames),
		PayloadLen: uint64(len(payload)),
		PayloadCRC: crc32.ChecksumIEEE(payload),
		PSNR:       psnrSum / float64(meta.Frames),
	}
	headerBytes := header.MarshalBinary()
	out := make([]byte, 0, len(headerBytes)+len(payload)+4)
	out = append(out, headerBytes...)
	out = append(out, payload...)
	fileCRC := crc32.ChecksumIEEE(out)
	var trailer [4]byte
	binary.LittleEndian.PutUint32(trailer[:], fileCRC)
	out = append(out, trailer[:]...)
	return &SegmentData{Bytes: out, Meta: meta, Bits: uint64(len(out)) * 8, PSNR: header.PSNR, CRC: fileCRC, FrameHashes: hashes}, nil
}

func framePSNR(a, b []byte) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var sum float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	mse := sum / float64(len(a))
	if mse == 0 {
		return 99
	}
	return 10 * math.Log10(255*255/mse)
}
