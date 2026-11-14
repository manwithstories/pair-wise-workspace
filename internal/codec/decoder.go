package codec

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"

	"tcr/internal/yuv"
)

// Segment is a fully decoded segment container.
type Segment struct {
	Header SegmentHeader
	Frames []*yuv.Frame
	QPs    []int
}

type byteReader struct {
	buf []byte
	pos int
}

func (r *byteReader) uvarint() (uint64, error) {
	v, n := binary.Uvarint(r.buf[r.pos:])
	if n <= 0 {
		return 0, fmt.Errorf("truncated or invalid uvarint at offset %d", r.pos)
	}
	r.pos += n
	return v, nil
}

func (r *byteReader) varint() (int64, error) {
	v, n := binary.Varint(r.buf[r.pos:])
	if n <= 0 {
		return 0, fmt.Errorf("truncated or invalid varint at offset %d", r.pos)
	}
	r.pos += n
	return v, nil
}

// ReadSegmentFile decodes a container file back into frames.
func ReadSegmentFile(path string) (*Segment, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < len(Magic) || !bytes.Equal(raw[:len(Magic)], []byte(Magic)) {
		return nil, fmt.Errorf("%s: not a TCR container (bad magic)", path)
	}
	r := &byteReader{buf: raw, pos: len(Magic)}
	fields := make([]uint64, 8)
	for i := range fields {
		v, err := r.uvarint()
		if err != nil {
			return nil, fmt.Errorf("%s: header: %w", path, err)
		}
		fields[i] = v
	}
	if fields[0] != Version {
		return nil, fmt.Errorf("%s: unsupported container version %d", path, fields[0])
	}
	hdr := SegmentHeader{
		SegIndex:   int(fields[1]),
		FrameCount: int(fields[2]),
		W:          int(fields[3]),
		H:          int(fields[4]),
		FPS:        int(fields[5]),
		TargetKbps: int(fields[6]),
		RawLen:     int(fields[7]),
	}
	if hdr.FrameCount <= 0 {
		return nil, fmt.Errorf("%s: container declares zero frames", path)
	}
	payload, err := rangeDecompress(raw[r.pos:], hdr.RawLen)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	r = &byteReader{buf: payload}
	seg := &Segment{Header: hdr}
	for i := 0; i < hdr.FrameCount; i++ {
		qp, err := r.uvarint()
		if err != nil {
			return nil, fmt.Errorf("%s: frame %d: %w", path, i, err)
		}
		f, err := yuv.NewFrame(hdr.W, hdr.H)
		if err != nil {
			return nil, err
		}
		for _, p := range []struct {
			plane []byte
			w, h  int
		}{{f.Y, f.W, f.H}, {f.Cb, f.ChromaW(), f.ChromaH()}, {f.Cr, f.ChromaW(), f.ChromaH()}} {
			if err := decodePlane(r, p.plane, p.w, p.h, int(qp)); err != nil {
				return nil, fmt.Errorf("%s: frame %d: %w", path, i, err)
			}
		}
		seg.Frames = append(seg.Frames, f)
		seg.QPs = append(seg.QPs, int(qp))
	}
	return seg, nil
}

func decodePlane(r *byteReader, plane []byte, w, h, qp int) error {
	pw, ph := (w+7)&^7, (h+7)&^7
	padded := make([]byte, pw*ph)
	var quant [64]int16
	var coeffs, block [64]float64
	for by := 0; by < ph; by += 8 {
		for bx := 0; bx < pw; bx += 8 {
			quant = [64]int16{}
			pos := 0
			for {
				run, err := r.uvarint()
				if err != nil {
					return err
				}
				if run == 0 {
					break
				}
				level, err := r.varint()
				if err != nil {
					return err
				}
				pos += int(run - 1)
				if pos >= 64 {
					return fmt.Errorf("run-length overruns 8x8 block")
				}
				quant[zigzag[pos]] = int16(level)
				pos++
			}
			dequantizeBlock(&quant, qp, &coeffs)
			idct8x8(&coeffs, &block)
			for y := 0; y < 8; y++ {
				row := (by+y)*pw + bx
				for x := 0; x < 8; x++ {
					v := block[y*8+x] + 128
					if v < 0 {
						v = 0
					} else if v > 255 {
						v = 255
					}
					padded[row+x] = byte(v + 0.5)
				}
			}
		}
	}
	for y := 0; y < h; y++ {
		copy(plane[y*w:y*w+w], padded[y*pw:y*pw+w])
	}
	return nil
}
