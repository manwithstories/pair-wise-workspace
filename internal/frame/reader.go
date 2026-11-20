// Package frame defines the on-disk frame record format and a streaming
// reader for it. The format is deliberately self-delimiting so a damaged
// stream can be sliced back into records and the exact byte offset of the
// first bad record can be reported:
//
//	+--------+--------+--------+----------------------------+--------+
//	| magic  | version| length | body (length bytes)        | crc32  |
//	| 4 B    | 1 B    | 4 B BE | header fields + payload    | 4 B BE |
//	+--------+--------+--------+----------------------------+--------+
//
// Body layout:
//
//	camera id  (2 B len + bytes)
//	sequence   (8 B BE)
//	device ts  (8 B BE)
//	rate       (4 B BE)
//	flags      (4 B BE: bit0 keyframe)
//	payload    (length - fixed - camera id bytes)
//
// The SHA-256 of the payload travels in a sidecar metadata table produced by
// MetadataWriter so large payloads never need to be held twice in memory. The
// per-record CRC32 protects the framing itself and is cheap enough to verify
// on every read; the SHA-256 protects payload integrity end to end.
package frame

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/mediamend/mediamend/internal/fault"
)

// Magic identifies a frame record stream.
var Magic = [4]byte{'P', 'W', 'F', 'R'}

// Version is the only record version this build understands.
const Version byte = 1

const (
	headerSize  = 4 + 1 + 4 // magic + version + length
	trailerSize = 4         // crc32
	// fixedBody is sequence+ts+rate+flags.
	fixedBody = 8 + 8 + 4 + 4
	// MaxRecord bounds one record including payload; protects memory.
	DefaultMaxRecord = 8 << 20 // 8 MiB
	// MaxCameraID bounds the camera identifier length.
	MaxCameraID = 255
)

// Flags carry per-frame properties.
type Flags uint32

// FlagKeyframe marks a frame that can start a segment.
const FlagKeyframe Flags = 1 << 0

// Has reports whether all bits in f are set.
func (fl Flags) Has(f Flags) bool { return fl&f == f }

// Header is the parsed fixed part of a record.
type Header struct {
	Camera   string
	Seq      uint64
	DeviceTS uint64
	Rate     uint32
	Flags    Flags
}

// Record is one fully read frame. Payload aliases the reader's buffer and is
// only valid until the next call to Read.
type Record struct {
	Header
	Payload []byte
	// Offset is the byte position of the record's magic in the stream.
	Offset int64
	// Length is the declared body length (excludes header and trailer).
	Length uint32
	// StoredSHA is filled in from the metadata sidecar by higher layers.
	StoredSHA string
}

// Keyframe reports whether the record is a segment start.
func (r *Record) Keyframe() bool { return r.Flags.Has(FlagKeyframe) }

// ErrBadFraming is returned after a fatal framing error; the stream cannot be
// resynchronised past a corrupted length, so the reader stops.
var ErrBadFraming = fmt.Errorf("frame: unrecoverable framing error")

// Reader streams records from an io.Reader with a bounded buffer.
type Reader struct {
	r      *bufio.Reader
	cam    string // expected camera; when set, mismatches are reported
	offset int64
	max    uint32
	buf    []byte
	faults fault.List
	done   bool
}

// NewReader creates a streaming record reader. maxRecord caps one record's
// declared body length; zero selects DefaultMaxRecord.
func NewReader(r io.Reader, camera string, maxRecord uint32) *Reader {
	if maxRecord == 0 {
		maxRecord = DefaultMaxRecord
	}
	return &Reader{
		r:   bufio.NewReaderSize(r, 64<<10),
		cam: camera,
		max: maxRecord,
		buf: make([]byte, 64<<10),
	}
}

// Faults returns the faults observed so far.
func (r *Reader) Faults() fault.List { return r.faults }

func (r *Reader) addFault(k fault.Kind, fatal bool, seq uint64, offset int64, msg string) {
	r.faults = append(r.faults, fault.Fault{
		Kind: k, Stage: fault.StageParse, Camera: r.cam,
		Seq: seq, Offset: offset, Message: msg, Fatal: fatal,
	})
}

// Read returns the next record. It returns io.EOF cleanly at the end of the
// stream and ErrBadFraming after a framing error that makes resynchronisation
// unsafe. A length that exceeds the cap or runs past the end of the stream is
// reported with the offending record's offset.
func (r *Reader) Read() (*Record, error) {
	if r.done {
		return nil, io.EOF
	}
	start := r.offset
	var hdr [headerSize]byte
	n, err := io.ReadFull(r.r, hdr[:])
	r.offset += int64(n)
	if err == io.EOF && n == 0 {
		return nil, io.EOF
	}
	if err == io.ErrUnexpectedEOF || (err == io.EOF && n > 0) {
		r.done = true
		r.addFault(fault.KindTruncatedRecord, true, 0, start,
			fmt.Sprintf("stream ended inside the record header (%d/%d bytes)", n, headerSize))
		return nil, ErrBadFraming
	}
	if err != nil {
		r.done = true
		return nil, err
	}
	if [4]byte{hdr[0], hdr[1], hdr[2], hdr[3]} != Magic {
		r.done = true
		r.addFault(fault.KindMalformedMetadata, true, 0, start, "bad record magic")
		return nil, ErrBadFraming
	}
	version := hdr[4]
	if version != Version {
		r.done = true
		r.addFault(fault.KindUnknownVersion, true, 0, start,
			fmt.Sprintf("unsupported record version %d (want %d)", version, Version))
		return nil, ErrBadFraming
	}
	length := binary.BigEndian.Uint32(hdr[5:9])
	// Bounds: the declared length must fit the cap and its minimum body size.
	if length > r.max {
		r.done = true
		r.addFault(fault.KindLengthOutOfBounds, true, 0, start,
			fmt.Sprintf("declared length %d exceeds cap %d", length, r.max))
		return nil, ErrBadFraming
	}
	if length < fixedBody+2 {
		r.done = true
		r.addFault(fault.KindLengthOutOfBounds, true, 0, start,
			fmt.Sprintf("declared length %d is below the minimum body size %d", length, fixedBody+2))
		return nil, ErrBadFraming
	}

	need := int(length) + trailerSize
	if cap(r.buf) < need {
		r.buf = make([]byte, need)
	}
	body := r.buf[:need]
	n, err = io.ReadFull(r.r, body)
	r.offset += int64(n)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		r.done = true
		r.addFault(fault.KindTruncatedRecord, true, 0, start,
			fmt.Sprintf("record truncated: %d of %d bytes available", n, need))
		return nil, ErrBadFraming
	}
	if err != nil {
		r.done = true
		return nil, err
	}

	// CRC covers the body only; the trailer is its expected value.
	want := binary.BigEndian.Uint32(body[length:])
	if got := crc32.ChecksumIEEE(body[:length]); got != want {
		// The body length was plausible, so the framing stays in sync and
		// the next record can still be read. This frame is dropped.
		r.addFault(fault.KindChecksumMismatch, false, 0, start,
			fmt.Sprintf("record CRC32 mismatch: stored %08x computed %08x", want, got))
		return &Record{Offset: start, Length: length}, errSkipped
	}

	rec, derr := decodeBody(body[:length], start)
	if derr != nil {
		// A structurally invalid body is frame-local: if the camera id fits
		// the declared length we can keep streaming.
		r.addFault(fault.KindMalformedMetadata, false, rec.Seq, start, derr.Error())
		return rec, errSkipped
	}
	if r.cam != "" && rec.Camera != r.cam {
		r.addFault(fault.KindMalformedMetadata, false, rec.Seq, start,
			fmt.Sprintf("record camera %q does not match stream camera %q", rec.Camera, r.cam))
	}
	return rec, nil
}

// errSkipped signals that a record was consumed but must not be processed.
// Read never returns it to callers; it is mapped to a nil record internally.
var errSkipped = fmt.Errorf("frame: skipped record")

// Next is Read with skipped records filtered out, which is what most callers
// want.
func (r *Reader) Next() (*Record, error) {
	for {
		rec, err := r.Read()
		if err == errSkipped {
			continue
		}
		return rec, err
	}
}

func decodeBody(b []byte, offset int64) (*Record, error) {
	rec := &Record{Offset: offset, Length: uint32(len(b))}
	if len(b) < fixedBody+2 {
		return rec, fmt.Errorf("body too short: %d bytes", len(b))
	}
	camLen := int(binary.BigEndian.Uint16(b[0:2]))
	if camLen > MaxCameraID || 2+camLen+fixedBody > len(b) {
		return rec, fmt.Errorf("camera id length %d exceeds body", camLen)
	}
	rec.Camera = string(b[2 : 2+camLen])
	p := b[2+camLen:]
	rec.Seq = binary.BigEndian.Uint64(p[0:8])
	rec.DeviceTS = binary.BigEndian.Uint64(p[8:16])
	rec.Rate = binary.BigEndian.Uint32(p[16:20])
	rec.Flags = Flags(binary.BigEndian.Uint32(p[20:24]))
	rec.Payload = p[24:]
	return rec, nil
}

// Encode appends one record to dst and returns the extended slice. It is the
// single source of truth for the wire format; the fixture generator and unit
// tests both use it.
func Encode(dst []byte, h Header, payload []byte) []byte {
	cam := h.Camera
	if len(cam) > MaxCameraID {
		cam = cam[:MaxCameraID]
	}
	length := 2 + len(cam) + fixedBody + len(payload)
	start := len(dst)
	dst = append(dst, Magic[:]...)
	dst = append(dst, Version)
	dst = binary.BigEndian.AppendUint32(dst, uint32(length))
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(cam)))
	dst = append(dst, cam...)
	dst = binary.BigEndian.AppendUint64(dst, h.Seq)
	dst = binary.BigEndian.AppendUint64(dst, h.DeviceTS)
	dst = binary.BigEndian.AppendUint32(dst, h.Rate)
	dst = binary.BigEndian.AppendUint32(dst, uint32(h.Flags))
	dst = append(dst, payload...)
	crc := crc32.ChecksumIEEE(dst[start+headerSize:])
	dst = binary.BigEndian.AppendUint32(dst, crc)
	return dst
}
