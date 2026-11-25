// Package ebml implements the subset of the EBML format needed to read
// Matroska and WebM containers: variable-length integer IDs and sizes,
// unknown-size elements, and bounded element iteration.
package ebml

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"mediapipe/internal/media"
)

// ID is an EBML element ID, including its marker bits as written on disk.
type ID uint32

// SizeUnknown is the sentinel value used by readers to mean "element runs to
// the end of its parent" (the all-ones VINT payload).
const SizeUnknown int64 = -1

// Common Matroska / WebM element IDs used by the demuxer.
const (
	IDEBML               ID = 0x1A45DFA3
	IDEBMLVersion        ID = 0x4286
	IDEBMLReadVersion    ID = 0x42F7
	IDEBMLMaxIDLength    ID = 0x42F2
	IDEBMLMaxSizeLength  ID = 0x42F3
	IDDocType            ID = 0x4282
	IDDocTypeVersion     ID = 0x4287
	IDSegment            ID = 0x18538067
	IDSeekHead           ID = 0x114D9B74
	IDInfo               ID = 0x1549A966
	IDTimestampScale     ID = 0x2AD7B1
	IDDuration           ID = 0x4489
	IDMuxingApp          ID = 0x4D80
	IDWritingApp         ID = 0x5741
	IDTracks             ID = 0x1654AE6B
	IDTrackEntry         ID = 0xAE
	IDTrackNumber        ID = 0xD7
	IDTrackUID           ID = 0x73C5
	IDTrackType          ID = 0x83
	IDFlagLacing         ID = 0x9C
	IDDefaultDuration    ID = 0x23E383
	IDLanguage           ID = 0x22B59C
	IDCodecID            ID = 0x86
	IDCodecPrivate       ID = 0x63A2
	IDCodecDelay         ID = 0x56AA
	IDSeekPreRoll        ID = 0x56BB
	IDVideo              ID = 0xE0
	IDPixelWidth         ID = 0xB0
	IDPixelHeight        ID = 0xBA
	IDAudio              ID = 0xE1
	IDChannels           ID = 0x9F
	IDSamplingFrequency  ID = 0xB5
	IDCluster            ID = 0x1F43B675
	IDTimestamp          ID = 0xE7
	IDSimpleBlock        ID = 0xA3
	IDBlockGroup         ID = 0xA0
	IDBlock              ID = 0xA1
	IDBlockDuration      ID = 0x9B
	IDReferenceBlock     ID = 0xFB
	IDCues               ID = 0x1C53BB6B
	IDTags               ID = 0x1254C367
	IDCRC32              ID = 0xBF
	IDVoid               ID = 0xEC
	IDClusterTimestamp   ID = IDTimestamp
	IDPrevSize           ID = 0xAB
	IDPosition           ID = 0xA7
	IDSilentTracks       ID = 0x5854
	IDInfoSegmentUID     ID = 0x73A4
	IDDurationScaledCode ID = 0x4489
)

// Limits bounds the resources a hostile or damaged file can consume.
type Limits struct {
	// MaxDepth is the maximum element nesting depth (segment=1).
	MaxDepth int
	// MaxElementSize is the largest element payload the reader will accept.
	MaxElementSize int64
	// MaxTrackCount is enforced by the demuxer, but kept here so callers can
	// share one limits value.
	MaxTrackCount int
	// MaxIDLength is the longest legal EBML ID in bytes (4 per EBML spec).
	MaxIDLength int
	// MaxSizeLength is the longest legal EBML size VINT in bytes.
	MaxSizeLength int
}

// DefaultLimits returns conservative broadcast-ingest limits.
func DefaultLimits() Limits {
	return Limits{
		MaxDepth:       16,
		MaxElementSize: 256 << 20,
		MaxTrackCount:  64,
		MaxIDLength:    4,
		MaxSizeLength:  8,
	}
}

func (l Limits) normalized() Limits {
	if l.MaxDepth <= 0 {
		l.MaxDepth = 64
	}
	if l.MaxElementSize <= 0 {
		l.MaxElementSize = 1 << 30
	}
	if l.MaxTrackCount <= 0 {
		l.MaxTrackCount = 256
	}
	if l.MaxIDLength <= 0 {
		l.MaxIDLength = 4
	}
	if l.MaxSizeLength <= 0 {
		l.MaxSizeLength = 8
	}
	return l
}

// Element describes an element header as read from the stream.
type Element struct {
	ID      ID
	Offset  int64 // offset of the first ID byte
	Header  int64 // ID bytes + size bytes
	Payload int64 // payload size in bytes
	Unknown bool  // true when the size VINT signalled "unknown"
}

// DataOffset is the absolute offset of the element payload.
func (e Element) DataOffset() int64 { return e.Offset + e.Header }

// End returns the offset just past the element, or -1 when the size is
// unknown.
func (e Element) End() int64 {
	if e.Unknown {
		return -1
	}
	return e.Offset + e.Header + e.Payload
}

func (e Element) String() string {
	if e.Unknown {
		return fmt.Sprintf("%#x@%d[size=unknown]", uint32(e.ID), e.Offset)
	}
	return fmt.Sprintf("%#x@%d[size=%d]", uint32(e.ID), e.Offset, e.Payload)
}

// Reader reads EBML elements sequentially while tracking a byte offset for
// diagnostics. It is not safe for concurrent use.
type Reader struct {
	r      io.Reader
	off    int64
	limits Limits
}

// NewReader wraps r with EBML parsing using DefaultLimits.
func NewReader(r io.Reader) *Reader { return NewReaderWithLimits(r, DefaultLimits()) }

// NewReaderWithLimits wraps r with explicit resource limits.
func NewReaderWithLimits(r io.Reader, limits Limits) *Reader {
	return &Reader{r: r, limits: limits.normalized()}
}

// Offset reports the current absolute stream offset.
func (r *Reader) Offset() int64 { return r.off }

// Limits returns the effective limits in force.
func (r *Reader) Limits() Limits { return r.limits }

// SetOffset overrides the tracked offset. It is used when a reader is
// constructed over a section of a larger file (for example after seeking to a
// cluster).
func (r *Reader) SetOffset(off int64) { r.off = off }

func (r *Reader) readByte() (byte, error) {
	var buf [1]byte
	n, err := io.ReadFull(r.r, buf[:])
	r.off += int64(n)
	if err != nil {
		return 0, media.At("ebml.read", r.off, media.ClassFatal, "truncated element header")
	}
	return buf[0], nil
}

// ReadFull reads exactly len(p) bytes or reports a truncation at the offset
// where data ran out.
func (r *Reader) ReadFull(p []byte) error {
	n, err := io.ReadFull(r.r, p)
	r.off += int64(n)
	if err != nil {
		return media.At("ebml.read", r.off, media.ClassFatal,
			"truncated payload: wanted %d bytes, got %d", len(p), n)
	}
	return nil
}

// readVint reads a variable-length integer, returning the payload value, the
// length in bytes and the raw true width. The returned value keeps the marker
// bit when keepMarker is true (used for element IDs).
func (r *Reader) readVint(keepMarker bool, maxLen int, what string) (uint64, int, error) {
	start := r.off
	first, err := r.readByte()
	if err != nil {
		return 0, 0, err
	}
	if first == 0 {
		return 0, 0, media.At("ebml.vint", start, media.ClassFatal,
			"invalid %s: leading byte 0x00 needs a 9-byte VINT", what)
	}
	length := 1
	mask := byte(0x80)
	for first&mask == 0 {
		length++
		mask >>= 1
	}
	if length > maxLen {
		return 0, 0, media.At("ebml.vint", start, media.ClassFatal,
			"overflowing %s: length %d exceeds limit %d", what, length, maxLen)
	}
	var value uint64
	if keepMarker {
		value = uint64(first)
	} else {
		value = uint64(first &^ mask)
	}
	var rest [7]byte
	if length > 1 {
		buf := rest[:length-1]
		if err := r.ReadFull(buf); err != nil {
			return 0, 0, media.At("ebml.vint", start, media.ClassFatal,
				"truncated %s of %d bytes", what, length)
		}
		for _, b := range buf {
			value = value<<8 | uint64(b)
		}
	}
	return value, length, nil
}

// ReadID reads an element ID, returning its byte length as well. IDs are
// limited to 4 bytes per EBML.
func (r *Reader) ReadID() (ID, int, error) {
	v, n, err := r.readVint(true, r.limits.MaxIDLength, "element ID")
	if err != nil {
		return 0, 0, err
	}
	if v > math.MaxUint32 {
		return 0, 0, media.At("ebml.id", r.off, media.ClassFatal, "element ID %#x exceeds 32 bits", v)
	}
	return ID(v), n, nil
}

// ReadSize reads an element size VINT. When the payload is all ones the
// element has unknown size and SizeUnknown is returned with unknown=true.
func (r *Reader) ReadSize() (size int64, unknown bool, length int, err error) {
	start := r.off
	v, n, err := r.readVint(false, r.limits.MaxSizeLength, "element size")
	if err != nil {
		return 0, false, 0, err
	}
	allOnes := uint64(1)<<(7*uint(n)) - 1
	if v == allOnes {
		return SizeUnknown, true, n, nil
	}
	if v > math.MaxInt64 {
		return 0, false, n, media.At("ebml.size", start, media.ClassFatal,
			"element size %d overflows int64", v)
	}
	if int64(v) > r.limits.MaxElementSize {
		return 0, false, n, media.At("ebml.size", start, media.ClassFatal,
			"element size %d exceeds limit %d", v, r.limits.MaxElementSize)
	}
	return int64(v), false, n, nil
}

// ReadElement reads the next element header.
func (r *Reader) ReadElement() (Element, error) {
	start := r.off
	id, idLen, err := r.ReadID()
	if err != nil {
		return Element{}, err
	}
	size, unknown, sizeLen, err := r.ReadSize()
	if err != nil {
		return Element{}, err
	}
	return Element{
		ID:      id,
		Offset:  start,
		Header:  int64(idLen + sizeLen),
		Payload: size,
		Unknown: unknown,
	}, nil
}

// VintLength returns the encoded length in bytes of a VINT whose first byte is
// given as part of value. For IDs value includes the marker bit; for sizes the
// caller can pass value|marker.
func VintLength(value uint64) int {
	var first byte
	for shift := 56; shift >= 0; shift -= 8 {
		b := byte(value >> uint(shift))
		if b != 0 {
			first = b
			break
		}
	}
	if first == 0 {
		return 9
	}
	n := 1
	for mask := byte(0x80); mask != 0 && first&mask == 0; mask >>= 1 {
		n++
	}
	return n
}

// ReadUint reads a big-endian unsigned integer of width 1..8 bytes.
func (r *Reader) ReadUint(width int) (uint64, error) {
	if width < 1 || width > 8 {
		return 0, media.At("ebml.uint", r.off, media.ClassFatal, "invalid integer width %d", width)
	}
	var buf [8]byte
	if err := r.ReadFull(buf[8-width:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(buf[:]), nil
}

// ReadInt reads a big-endian signed integer of width 1..8 bytes.
func (r *Reader) ReadInt(width int) (int64, error) {
	u, err := r.ReadUint(width)
	if err != nil {
		return 0, err
	}
	if width < 8 {
		shift := uint(64 - 8*width)
		return int64(u<<shift) >> shift, nil
	}
	return int64(u), nil
}

// ReadFloat reads a 4- or 8-byte IEEE float.
func (r *Reader) ReadFloat(width int) (float64, error) {
	switch width {
	case 4:
		var buf [4]byte
		if err := r.ReadFull(buf[:]); err != nil {
			return 0, err
		}
		return float64(math.Float32frombits(binary.BigEndian.Uint32(buf[:]))), nil
	case 8:
		var buf [8]byte
		if err := r.ReadFull(buf[:]); err != nil {
			return 0, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(buf[:])), nil
	default:
		return 0, media.At("ebml.float", r.off, media.ClassFatal, "invalid float width %d", width)
	}
}

// ReadString reads a UTF-8 string payload, trimming trailing NUL padding.
func (r *Reader) ReadString(size int64) (string, error) {
	b, err := r.ReadBytes(size)
	if err != nil {
		return "", err
	}
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return string(b), nil
}

// ReadBytes reads size bytes, enforcing MaxElementSize.
func (r *Reader) ReadBytes(size int64) ([]byte, error) {
	if size < 0 {
		return nil, media.At("ebml.bytes", r.off, media.ClassFatal, "negative payload size")
	}
	if size > r.limits.MaxElementSize {
		return nil, media.At("ebml.bytes", r.off, media.ClassFatal,
			"payload size %d exceeds limit %d", size, r.limits.MaxElementSize)
	}
	buf := make([]byte, size)
	if err := r.ReadFull(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// Skip advances over size bytes without buffering them.
func (r *Reader) Skip(size int64) error {
	if size < 0 {
		return media.At("ebml.skip", r.off, media.ClassFatal, "negative skip size")
	}
	n, err := io.CopyN(io.Discard, r.r, size)
	r.off += n
	if err != nil {
		return media.At("ebml.skip", r.off, media.ClassFatal,
			"truncated skip: wanted %d bytes, got %d", size, n)
	}
	return nil
}

// ReadCRC32 reads an EBML CRC-32 element payload. Matroska stores it in the
// same polynomial IEEE CRC-32 uses.
func (r *Reader) ReadCRC32(el Element) (uint32, error) {
	b, err := r.ReadBytes(el.Payload)
	if err != nil {
		return 0, err
	}
	if len(b) != 4 {
		return 0, media.At("ebml.crc", el.Offset, media.ClassFatal, "CRC-32 element has %d bytes", len(b))
	}
	return binary.BigEndian.Uint32(b), nil
}
