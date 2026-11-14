package codec

import (
	"encoding/binary"
	"fmt"
	"os"

	"tcr/internal/yuv"
)

// Container magic and version for segment files.
const (
	Magic   = "TCR1"
	Version = 2
)

// SegmentHeader is stored at the start of every segment container.
type SegmentHeader struct {
	SegIndex   int
	FrameCount int
	W, H       int
	FPS        int
	TargetKbps int
	RawLen     int // uncompressed payload length in bytes
}

// SegmentFileName returns the canonical on-disk name for a segment.
func SegmentFileName(idx int) string { return fmt.Sprintf("seg_%02d.tcr", idx) }

// padPlane copies a plane into a buffer padded to a multiple of 8
// per dimension, replicating edge samples.
func padPlane(plane []byte, w, h int) ([]byte, int, int) {
	pw, ph := (w+7)&^7, (h+7)&^7
	if pw == w && ph == h {
		return plane, w, h
	}
	padded := make([]byte, pw*ph)
	for y := 0; y < ph; y++ {
		sy := y
		if sy >= h {
			sy = h - 1
		}
		row := padded[y*pw : y*pw+pw]
		copy(row, plane[sy*w:sy*w+w])
		for x := w; x < pw; x++ {
			row[x] = row[w-1]
		}
	}
	return padded, pw, ph
}

// encodePlane DCT-transforms, quantizes and entropy-codes one plane.
// Entropy coding: zigzag scan, then (run, level) pairs as uvarint(run+1)
// plus zigzag-varint(level), terminated by uvarint(0) (end of block).
func encodePlane(dst []byte, plane []byte, w, h, qp int) []byte {
	padded, pw, ph := padPlane(plane, w, h)
	var block, coeffs [64]float64
	var quant [64]int16
	for by := 0; by < ph; by += 8 {
		for bx := 0; bx < pw; bx += 8 {
			for y := 0; y < 8; y++ {
				row := (by+y)*pw + bx
				for x := 0; x < 8; x++ {
					block[y*8+x] = float64(padded[row+x]) - 128
				}
			}
			dct8x8(&block, &coeffs)
			quantizeBlock(&coeffs, qp, &quant)
			dst = appendBlockRLE(dst, &quant)
		}
	}
	return dst
}

func appendBlockRLE(dst []byte, q *[64]int16) []byte {
	run := 0
	for i := 0; i < 64; i++ {
		v := q[zigzag[i]]
		if v == 0 {
			run++
			continue
		}
		dst = binary.AppendUvarint(dst, uint64(run+1))
		dst = binary.AppendVarint(dst, int64(v))
		run = 0
	}
	return binary.AppendUvarint(dst, 0) // end of block
}

// EncodeFrame appends one frame (QP + three planes) to dst.
func EncodeFrame(dst []byte, f *yuv.Frame, qp int) []byte {
	dst = binary.AppendUvarint(dst, uint64(qp))
	dst = encodePlane(dst, f.Y, f.W, f.H, qp)
	dst = encodePlane(dst, f.Cb, f.ChromaW(), f.ChromaH(), qp)
	return encodePlane(dst, f.Cr, f.ChromaW(), f.ChromaH(), qp)
}

func appendHeader(dst []byte, hdr SegmentHeader) []byte {
	dst = append(dst, Magic...)
	for _, v := range []int{Version, hdr.SegIndex, hdr.FrameCount, hdr.W, hdr.H, hdr.FPS, hdr.TargetKbps, hdr.RawLen} {
		dst = binary.AppendUvarint(dst, uint64(v))
	}
	return dst
}

// encodePayload serializes all frames into one uncompressed byte stream.
func encodePayload(frames []*yuv.Frame, qps []int) []byte {
	buf := make([]byte, 0, 1<<16)
	for i, f := range frames {
		buf = EncodeFrame(buf, f, qps[i])
	}
	return buf
}

// MeasureBits returns the encoded size in bits for frames at the given
// per-frame QPs, including container overhead, without touching disk.
func MeasureBits(frames []*yuv.Frame, qps []int, hdr SegmentHeader) int64 {
	payload := encodePayload(frames, qps)
	hdr.RawLen = len(payload)
	out := appendHeader(nil, hdr)
	out = append(out, rangeCompress(payload)...)
	return int64(len(out)) * 8
}

// WriteSegmentFile encodes frames into a container file and returns its size in bytes.
func WriteSegmentFile(path string, hdr SegmentHeader, frames []*yuv.Frame, qps []int) (int64, error) {
	if len(frames) == 0 {
		return 0, fmt.Errorf("cannot encode segment %d: zero frames", hdr.SegIndex)
	}
	if len(qps) != len(frames) {
		return 0, fmt.Errorf("qp list length %d != frame count %d", len(qps), len(frames))
	}
	payload := encodePayload(frames, qps)
	hdr.RawLen = len(payload)
	out := appendHeader(nil, hdr)
	out = append(out, rangeCompress(payload)...)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return 0, err
	}
	return int64(len(out)), nil
}
