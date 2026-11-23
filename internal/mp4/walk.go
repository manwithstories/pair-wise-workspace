// Package mp4 reads the box subset mediasync needs and copies coded samples
// out of mdat without interpretation. Truncated or malformed boxes are reported
// with the owning track and the byte offset of the offending header.
package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/abema/go-mp4"

	"mediasync/internal/model"
)

// maxSampleBytes caps a single sample payload (64 MiB block limit minus headroom).
const maxSampleBytes = 64<<20 - 1

// Input is the result of reading one MP4 file.
type Input struct {
	Path      string
	Tracks    []*model.Track
	Stats     []model.TrackStats
	FastStart bool
	// Truncated reports a structural failure with the offset that failed.
	Truncated *Truncation
}

// Truncation describes a box whose declared size overruns the input.
type Truncation struct {
	TrackType string
	TrackID   uint32
	BoxType   string
	Offset    uint64
	Declared  uint64
	Available uint64
}

// Reader walks an MP4 byte stream.
type Reader struct {
	src  io.ReaderAt
	size int64
}

// NewReader wraps an input.
func NewReader(src io.ReaderAt, size int64) *Reader {
	return &Reader{src: src, size: size}
}

type hdr struct {
	typ       string
	offset    uint64
	hdrSize   uint64
	payload   uint64
	total     uint64
	extendEOF bool
}

// kindForHandler maps an mdia handler type to a track kind.
func kindForHandler(h string) model.TrackKind {
	switch h {
	case "vide":
		return model.KindVideo
	case "soun":
		return model.KindAudio
	default:
		return model.KindData
	}
}

// rotationFromMatrix extracts the axis-aligned rotation from a tkhd matrix.
func rotationFromMatrix(m [9]int32) (int, bool) {
	const one = int32(0x10000)
	a, b := m[0], m[1]
	c, d := m[3], m[4]
	switch {
	case a == one && b == 0 && c == 0 && d == one:
		return 0, true
	case a == 0 && b == one && c == -one && d == 0:
		return 90, true
	case a == -one && b == 0 && c == 0 && d == -one:
		return 180, true
	case a == 0 && b == -one && c == one && d == 0:
		return 270, true
	}
	return 0, false
}

func be16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }
func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
func be64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }

// PayloadOffset returns the box payload start.
func (h hdr) PayloadOffset() uint64 { return h.offset + h.hdrSize }

// PayloadLen returns the payload length in bytes.
func (h hdr) PayloadLen() uint64 { return h.payload }

// classify converts a walk error into a coded error and records truncation
// details on the input.
func (r *Reader) classify(err error) error {
	var so *sizeOverrun
	if errors.As(err, &so) {
		return model.NewError(model.ErrTruncatedBox, 0, int64(so.offset),
			"box %q declares %d bytes, %d available", so.typ, so.declared, so.available)
	}
	var ce *model.CodedError
	if errors.As(err, &ce) {
		return ce
	}
	return model.WrapError(model.ErrTruncatedBox, 0, 0, err)
}

// unmarshal decodes a leaf box payload with go-mp4. The payload size is checked
// against the declared box size first.
func (r *Reader) unmarshal(h hdr, dst mp4.IBox, ctx *mp4.Context) error {
	buf, err := r.payload(h)
	if err != nil {
		return r.classify(err)
	}
	c := mp4.Context{}
	if ctx != nil {
		c = *ctx
	}
	if _, err := mp4.Unmarshal(newSliceReader(buf), uint64(len(buf)), dst, c); err != nil {
		return model.WrapError(model.ErrTruncatedBox, 0, int64(h.offset), fmt.Errorf("%s: %w", h.typ, err))
	}
	return nil
}

type sliceReader struct {
	buf []byte
	off int64
}

func newSliceReader(b []byte) *sliceReader { return &sliceReader{buf: b} }
func (s *sliceReader) Read(p []byte) (int, error) {
	if s.off >= int64(len(s.buf)) {
		return 0, errEOF
	}
	n := copy(p, s.buf[s.off:])
	s.off += int64(n)
	return n, nil
}
func (s *sliceReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case 0:
		s.off = off
	case 1:
		s.off += off
	case 2:
		s.off = int64(len(s.buf)) + off
	}
	if s.off < 0 {
		s.off = 0
	}
	return s.off, nil
}

var errEOF = errors.New("EOF")

// readHeader reads an 8- or 16-byte box header at off.
func (r *Reader) readHeader(off uint64) (hdr, error) {
	if off+8 > uint64(r.size) {
		return hdr{}, model.NewError(model.ErrTruncatedBox, 0, int64(off), "box header needs 8 bytes, %d remain", uint64(r.size)-off)
	}
	var buf [16]byte
	n := 16
	if int64(off)+16 > r.size {
		n = 8
	}
	if _, err := r.src.ReadAt(buf[:n], int64(off)); err != nil && err != io.EOF {
		return hdr{}, err
	}
	size := binary.BigEndian.Uint32(buf[0:4])
	typ := string(buf[4:8])
	h := hdr{typ: typ, offset: off, hdrSize: 8}
	switch size {
	case 0:
		h.extendEOF = true
		h.total = uint64(r.size) - off
	case 1:
		if n < 16 {
			return hdr{}, model.NewError(model.ErrTruncatedBox, 0, int64(off), "%s box requires 16-byte header", typ)
		}
		h.hdrSize = 16
		h.total = binary.BigEndian.Uint64(buf[8:16])
		if h.total > math.MaxInt64 {
			return hdr{}, model.NewError(model.ErrSizeLimit, 0, int64(off), "%s box length %d exceeds addressable size", typ, h.total)
		}
	default:
		h.total = uint64(size)
	}
	if h.total < h.hdrSize {
		return hdr{}, model.NewError(model.ErrTruncatedBox, 0, int64(off), "%s box declares %d bytes, smaller than its header", typ, h.total)
	}
	h.payload = h.total - h.hdrSize
	if h.payload > uint64(r.size) || off+h.total > uint64(r.size) {
		return hdr{}, &sizeOverrun{typ: typ, offset: off, declared: h.total, available: uint64(r.size) - off}
	}
	return h, nil
}

type sizeOverrun struct {
	typ       string
	offset    uint64
	declared  uint64
	available uint64
}

func (e *sizeOverrun) Error() string {
	return fmt.Sprintf("box %q at offset %d declares %d bytes but only %d remain", e.typ, e.offset, e.declared, e.available)
}

// boxesIn lists the direct children of a container payload.
func (r *Reader) boxesIn(start, end uint64) ([]hdr, error) {
	var out []hdr
	for off := start; off+8 <= end; {
		h, err := r.readHeader(off)
		if err != nil {
			return nil, err
		}
		if off+h.total > end {
			return nil, &sizeOverrun{typ: h.typ, offset: off, declared: h.total, available: end - off}
		}
		out = append(out, h)
		off += h.total
		if h.total == 0 {
			break
		}
	}
	return out, nil
}

func (r *Reader) payload(h hdr) ([]byte, error) {
	if h.payload > maxSampleBytes {
		return nil, model.NewError(model.ErrSizeLimit, 0, int64(h.offset), "%s payload %d exceeds limit", h.typ, h.payload)
	}
	buf := make([]byte, h.payload)
	if h.payload == 0 {
		return buf, nil
	}
	if _, err := r.src.ReadAt(buf, int64(h.offset+h.hdrSize)); err != nil && err != io.EOF {
		return nil, err
	}
	return buf, nil
}

func find(hs []hdr, typ string) (hdr, bool) {
	for _, h := range hs {
		if h.typ == typ {
			return h, true
		}
	}
	return hdr{}, false
}

func findAll(hs []hdr, typ string) []hdr {
	var out []hdr
	for _, h := range hs {
		if h.typ == typ {
			out = append(out, h)
		}
	}
	return out
}
