// Package ebml implements a streaming reader for the EBML framing used by
// Matroska and WebM. It never buffers whole elements: callers either read a
// small payload or leave the reader at the element's end and seek past it.
package ebml

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"archivecheck.local/archivecheck/internal/fault"
)

// Common Matroska element IDs. Only IDs that the pipeline inspects are named;
// everything else is reported numerically.
const (
	IDEBML               uint32 = 0x1A45DFA3
	IDEBMLVersion        uint32 = 0x4286
	IDEBMLReadVersion    uint32 = 0x42F7
	IDEBMLMaxIDLength    uint32 = 0x42F2
	IDEBMLMaxSizeLength  uint32 = 0x42F3
	IDDocType            uint32 = 0x4282
	IDDocTypeVersion     uint32 = 0x4287
	IDDocTypeReadVersion uint32 = 0x4285
	IDSegment            uint32 = 0x18538067
	IDSeekHead           uint32 = 0x114D9B74
	IDInfo               uint32 = 0x1549A966
	IDTimestampScale     uint32 = 0x2AD7B1
	IDDuration           uint32 = 0x4489
	IDMuxingApp          uint32 = 0x4D80
	IDWritingApp         uint32 = 0x5741
	IDTracks             uint32 = 0x1654AE6B
	IDTrackEntry         uint32 = 0xAE
	IDTrackNumber        uint32 = 0xD7
	IDTrackUID           uint32 = 0x73C5
	IDTrackType          uint32 = 0x83
	IDFlagLacing         uint32 = 0x9C
	IDName               uint32 = 0x536E
	IDLanguage           uint32 = 0x22B59C
	IDCodecID            uint32 = 0x86
	IDCodecPrivate       uint32 = 0x63A2
	IDCodecName          uint32 = 0x258688
	IDDefaultDuration    uint32 = 0x23E383
	IDVideo              uint32 = 0xE0
	IDPixelWidth         uint32 = 0xB0
	IDPixelHeight        uint32 = 0xBA
	IDAudio              uint32 = 0xE1
	IDSamplingFrequency  uint32 = 0xB5
	IDChannels           uint32 = 0x9F
	IDCluster            uint32 = 0x1F43B675
	IDTimestamp          uint32 = 0xE7
	IDSimpleBlock        uint32 = 0xA3
	IDBlockGroup         uint32 = 0xA0
	IDBlock              uint32 = 0xA1
	IDBlockDuration      uint32 = 0x9B
	IDReferenceBlock     uint32 = 0xFB
	IDKeyframe           uint32 = 0x9D
	IDDiscardPadding     uint32 = 0x75A2
	IDCues               uint32 = 0x1C53BB6B
	IDCuePoint           uint32 = 0xBB
	IDCueTime            uint32 = 0xB3
	IDCueTrackPositions  uint32 = 0xB7
	IDCueTrack           uint32 = 0xF7
	IDCueClusterPosition uint32 = 0xF1
	IDCueBlockNumber     uint32 = 0x5378
	IDTags               uint32 = 0x1254C367
	IDChapters           uint32 = 0x1043A770
	IDAttachments        uint32 = 0x1941A469
	IDVoid               uint32 = 0xEC
	IDCRC32              uint32 = 0xBF
)

var names = map[uint32]string{
	IDEBML: "EBML", IDEBMLVersion: "EBMLVersion", IDEBMLReadVersion: "EBMLReadVersion",
	IDEBMLMaxIDLength: "EBMLMaxIDLength", IDEBMLMaxSizeLength: "EBMLMaxSizeLength",
	IDDocType: "DocType", IDDocTypeVersion: "DocTypeVersion", IDDocTypeReadVersion: "DocTypeReadVersion",
	IDSegment: "Segment", IDSeekHead: "SeekHead", IDInfo: "Info", IDTimestampScale: "TimestampScale",
	IDDuration: "Duration", IDMuxingApp: "MuxingApp", IDWritingApp: "WritingApp",
	IDTracks: "Tracks", IDTrackEntry: "TrackEntry", IDTrackNumber: "TrackNumber", IDTrackUID: "TrackUID",
	IDTrackType: "TrackType", IDFlagLacing: "FlagLacing", IDName: "Name", IDLanguage: "Language",
	IDCodecID: "CodecID", IDCodecPrivate: "CodecPrivate", IDCodecName: "CodecName",
	IDDefaultDuration: "DefaultDuration", IDVideo: "Video", IDPixelWidth: "PixelWidth",
	IDPixelHeight: "PixelHeight", IDAudio: "Audio", IDSamplingFrequency: "SamplingFrequency",
	IDChannels: "Channels", IDCluster: "Cluster", IDTimestamp: "Timestamp",
	IDSimpleBlock: "SimpleBlock", IDBlockGroup: "BlockGroup", IDBlock: "Block",
	IDBlockDuration: "BlockDuration", IDReferenceBlock: "ReferenceBlock", IDKeyframe: "Keyframe",
	IDDiscardPadding: "DiscardPadding", IDCues: "Cues", IDCuePoint: "CuePoint", IDCueTime: "CueTime",
	IDCueTrackPositions: "CueTrackPositions", IDCueTrack: "CueTrack",
	IDCueClusterPosition: "CueClusterPosition", IDCueBlockNumber: "CueBlockNumber",
	IDTags: "Tags", IDChapters: "Chapters", IDAttachments: "Attachments", IDVoid: "Void", IDCRC32: "CRC32",
}

// Name returns the Matroska name for id, or a hexadecimal fallback.
func Name(id uint32) string {
	if n, ok := names[id]; ok {
		return n
	}
	return fmt.Sprintf("0x%X", id)
}

// Element is the header of one EBML element.
type Element struct {
	ID uint32
	// Offset is the absolute offset of the element's first ID byte.
	Offset int64
	// DataOffset is Offset plus the length of ID and size fields.
	DataOffset int64
	// HeaderSize is DataOffset-Offset.
	HeaderSize int64
	// Size is the payload size; it is -1 when the element has unknown size.
	Size int64
	// End is DataOffset+Size for known-size elements and -1 otherwise.
	End int64
	// UnknownSize reports an EBML unknown-size element.
	UnknownSize bool
}

// Name returns the element name.
func (e Element) Name() string { return Name(e.ID) }

// Reader reads EBML elements from a seekable source.
type Reader struct {
	rs  io.ReadSeeker
	off int64
}

// NewReader wraps rs. The reader starts at the source's current offset.
func NewReader(rs io.ReadSeeker) *Reader {
	off, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		off = 0
	}
	return &Reader{rs: rs, off: off}
}

// Offset returns the current offset.
func (r *Reader) Offset() int64 { return r.off }

// SeekTo moves the reader to an absolute offset.
func (r *Reader) SeekTo(off int64) error {
	if off < 0 {
		return fmt.Errorf("ebml: negative seek offset %d", off)
	}
	if _, err := r.rs.Seek(off, io.SeekStart); err != nil {
		return err
	}
	r.off = off
	return nil
}

// SourceSize returns the total source length.
func (r *Reader) SourceSize() (int64, error) {
	cur, err := r.rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	end, err := r.rs.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := r.rs.Seek(cur, io.SeekStart); err != nil {
		return 0, err
	}
	return end, nil
}

func (r *Reader) readByte(limit int64) (byte, error) {
	if r.off >= limit {
		return 0, io.EOF
	}
	var b [1]byte
	if _, err := io.ReadFull(r.rs, b[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, fault.At(fault.StageParse, r.off, ErrTruncated)
		}
		return 0, err
	}
	r.off++
	return b[0], nil
}

// Next reads the element header at the current offset. limit is the exclusive
// end of the enclosing scope: headers and declared payloads may not cross it.
func (r *Reader) Next(limit int64) (Element, error) {
	start := r.off
	first, err := r.readByte(limit)
	if err != nil {
		return Element{}, err
	}
	if first == 0 {
		return Element{}, fault.At(fault.StageParse, start, ErrVint)
	}
	idLen := vintLen(first)
	if idLen == 0 || idLen > 4 {
		return Element{}, fault.At(fault.StageParse, start, fmt.Errorf("%w: invalid ID length %d", ErrVint, idLen))
	}
	idBuf := [4]byte{first}
	if idLen > 1 {
		if err := r.readFull(idBuf[1:idLen], limit); err != nil {
			return Element{}, startErr(err, start)
		}
	}
	var id uint32
	for i := 0; i < idLen; i++ {
		id = id<<8 | uint32(idBuf[i])
	}

	szFirst, err := r.readByte(limit)
	if err != nil {
		return Element{}, startErr(err, start)
	}
	szLen := vintLen(szFirst)
	if szLen == 0 || szLen > 8 {
		return Element{}, fault.AtElement(fault.StageParse, start, Name(id), fmt.Errorf("%w: invalid size length %d", ErrVint, szLen))
	}
	szBuf := [8]byte{szFirst}
	if szLen > 1 {
		if err := r.readFull(szBuf[1:szLen], limit); err != nil {
			return Element{}, startErr(err, start)
		}
	}
	size := decodeSize(szBuf[:szLen], szLen)
	header := int64(idLen + szLen)
	e := Element{
		ID:         id,
		Offset:     start,
		DataOffset: start + header,
		HeaderSize: header,
	}
	if size == unknownSize(szLen) {
		e.UnknownSize = true
		e.Size = -1
		e.End = -1
		return e, nil
	}
	if size > uint64(1)<<62 {
		return Element{}, fault.AtElement(fault.StageParse, start, Name(id), fmt.Errorf("%w: implausible size %d", ErrOversized, size))
	}
	e.Size = int64(size)
	e.End = e.DataOffset + e.Size
	if e.End > limit || e.End < e.DataOffset {
		return Element{}, fault.AtElement(fault.StageParse, start, Name(id),
			fmt.Errorf("%w: element end %d exceeds limit %d", ErrOversized, e.End, limit))
	}
	return e, nil
}

func (r *Reader) readFull(dst []byte, limit int64) error {
	if r.off+int64(len(dst)) > limit {
		return fault.At(fault.StageParse, r.off, ErrTruncated)
	}
	if _, err := io.ReadFull(r.rs, dst); err != nil {
		return fault.At(fault.StageParse, r.off, ErrTruncated)
	}
	r.off += int64(len(dst))
	return nil
}

func startErr(err error, start int64) error {
	var fe *fault.Error
	if errors.As(err, &fe) {
		fe.Offset = start
		return fe
	}
	return fault.At(fault.StageParse, start, err)
}

func vintLen(b byte) int {
	switch {
	case b&0x80 != 0:
		return 1
	case b&0x40 != 0:
		return 2
	case b&0x20 != 0:
		return 3
	case b&0x10 != 0:
		return 4
	case b&0x08 != 0:
		return 5
	case b&0x04 != 0:
		return 6
	case b&0x02 != 0:
		return 7
	case b&0x01 != 0:
		return 8
	default:
		return 0
	}
}

func decodeSize(buf []byte, length int) uint64 {
	var v uint64
	for _, b := range buf[:length] {
		v = v<<8 | uint64(b)
	}
	// Clear the length marker bit.
	v &^= uint64(1) << (7 * uint(length))
	return v
}

func unknownSize(length int) uint64 {
	return (uint64(1) << (7 * uint(length))) - 1
}

// ReadUint decodes an unsigned integer payload of at most 8 bytes.
func (r *Reader) ReadUint(e Element) (uint64, error) {
	buf, err := r.ReadSmall(e, 8)
	if err != nil {
		return 0, err
	}
	if len(buf) == 0 {
		return 0, fault.AtElement(fault.StageParse, e.Offset, e.Name(), ErrMalformed)
	}
	var v uint64
	for _, b := range buf {
		v = v<<8 | uint64(b)
	}
	return v, nil
}

// ReadInt decodes a signed integer payload of at most 8 bytes.
func (r *Reader) ReadInt(e Element) (int64, error) {
	buf, err := r.ReadSmall(e, 8)
	if err != nil {
		return 0, err
	}
	if len(buf) == 0 || len(buf) > 8 {
		return 0, fault.AtElement(fault.StageParse, e.Offset, e.Name(), ErrMalformed)
	}
	var v uint64
	for _, b := range buf {
		v = v<<8 | uint64(b)
	}
	shift := uint(64 - 8*len(buf))
	return int64(v<<shift) >> shift, nil
}

// ReadSmall reads a payload of at most maxSize bytes. It is the only API that
// allocates, and it refuses larger payloads so that no element can force an
// unbounded allocation.
func (r *Reader) ReadSmall(e Element, maxSize int64) ([]byte, error) {
	if e.UnknownSize {
		return nil, fault.AtElement(fault.StageParse, e.Offset, e.Name(), ErrMalformed)
	}
	if e.Size > maxSize {
		return nil, fault.AtElement(fault.StageParse, e.Offset, e.Name(),
			fmt.Errorf("%w: payload %d exceeds %d", ErrOversized, e.Size, maxSize))
	}
	if err := r.SeekTo(e.DataOffset); err != nil {
		return nil, err
	}
	buf := make([]byte, e.Size)
	if e.Size > 0 {
		if err := r.readFull(buf, e.End); err != nil {
			return nil, startErr(err, e.Offset)
		}
	}
	return buf, nil
}

// ReadFull fills p from the element's payload at the current reader offset.
// The caller must have sought to the payload (for example with Seek).
func (r *Reader) ReadFull(p []byte) (int, error) {
	if _, err := io.ReadFull(r.rs, p); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, fault.At(fault.StageParse, r.off, ErrTruncated)
		}
		return 0, err
	}
	r.off += int64(len(p))
	return len(p), nil
}

// Skip advances to the element's end. Unknown-size elements cannot be skipped.
func (r *Reader) Skip(e Element) error {
	if e.UnknownSize {
		return fault.AtElement(fault.StageParse, e.Offset, e.Name(), ErrMalformed)
	}
	return r.SeekTo(e.End)
}

// Stream copies the element payload to w without buffering it. It returns the
// number of bytes copied. maxSize guards the caller's memory budget.
func (r *Reader) Stream(e Element, w io.Writer, maxSize int64) (int64, error) {
	if e.UnknownSize {
		return 0, fault.AtElement(fault.StageParse, e.Offset, e.Name(), ErrMalformed)
	}
	if maxSize > 0 && e.Size > maxSize {
		return 0, fault.AtElement(fault.StageParse, e.Offset, e.Name(),
			fmt.Errorf("%w: payload %d exceeds %d", ErrOversized, e.Size, maxSize))
	}
	if err := r.SeekTo(e.DataOffset); err != nil {
		return 0, err
	}
	n, err := io.CopyN(w, r.rs, e.Size)
	if err != nil {
		return n, fault.At(fault.StageParse, e.DataOffset+n, ErrTruncated)
	}
	r.off += n
	return n, nil
}

// Uint64 is a convenience wrapper for binary-decoding a small payload.
func Uint64(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// Float64 decodes a 4- or 8-byte big-endian IEEE float (Matroska float type).
func Float64(b []byte) (float64, error) {
	switch len(b) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), nil
	case 8:
		return float64FromBits(binary.BigEndian.Uint64(b)), nil
	case 0:
		return 0, nil
	default:
		return 0, errors.New("ebml: invalid float payload length")
	}
}
