// Package mkv decodes the streaming EBML element stream produced by
// internal/ebml into Matroska domain objects: track metadata and a block
// stream on the unified MediaTime base. It owns Matroska-specific value
// decoding (uint/float/string) and block header parsing.
package mkv

import (
	"encoding/binary"
	"errors"
	"io"
	"math"

	"review/internal/codec"
	"review/internal/ebml"
	"review/internal/media"
)

// Track is the decoded Matroska TrackEntry subset the pipeline needs.
type Track struct {
	Number       uint32
	UID          uint64
	Type         uint8
	Codec        string
	DefaultDurNS uint64
	Channels     uint32 // 0 means the element was absent (unknown layout)
	SampleRate   float64
	BitDepth     uint32
	Matrix       uint64 // 0 means absent
	Transfer     uint64
	Primaries    uint64
}

// Block is one decoded SimpleBlock/Block with its unified timestamp and
// source offset for error provenance.
type Block struct {
	Track    uint32
	Time     media.MediaTime
	Keyframe bool
	Offset   int64 // absolute byte offset of the block element
	Payload  []byte
}

// DecodeError wraps an EBML-level failure with Matroska context.
type DecodeError struct {
	Stage string
	Cause error
}

func (e *DecodeError) Error() string { return "mkv: " + e.Stage + ": " + e.Cause.Error() }
func (e *DecodeError) Unwrap() error { return e.Cause }

// DecodeUint parses a big-endian unsigned integer payload (1..8 bytes).
func DecodeUint(b []byte) (uint64, error) {
	if len(b) == 0 || len(b) > 8 {
		return 0, errors.New("mkv: uint element must be 1..8 bytes")
	}
	var v uint64
	for _, c := range b {
		v = (v << 8) | uint64(c)
	}
	return v, nil
}

// DecodeFloat parses a 0/4/8-byte Matroska float element. Zero-length
// payloads are legal and mean 0.0.
func DecodeFloat(b []byte) (float64, error) {
	switch len(b) {
	case 0:
		return 0, nil
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), nil
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
	default:
		return 0, errors.New("mkv: float element must be 0, 4 or 8 bytes")
	}
}

// DecodeBlock parses a SimpleBlock/Block payload. The track VINT is limited
// to one byte across our fixture range; wider values are rejected rather
// than guessed.
func DecodeBlock(raw []byte) (track uint32, relative int16, flags byte, body []byte, err error) {
	if len(raw) < 4 || raw[0]&0x80 == 0 {
		return 0, 0, 0, nil, errors.New("mkv: malformed block header")
	}
	tr, rel, fl, err := codec.DecodeBlockHeader(raw[:4])
	if err != nil {
		return 0, 0, 0, nil, err
	}
	return uint32(tr), rel, fl, raw[4:], nil
}

// walkChildren iterates the current master frame, invoking fn for every
// child header. It closes the frame on io.EOF; any other error aborts.
func walkChildren(rd *ebml.Reader, enter ebml.Header, fn func(ebml.Header) error) error {
	if err := rd.Enter(enter); err != nil {
		return &DecodeError{Stage: "enter", Cause: err}
	}
	for {
		h, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return rd.Leave()
		}
		if err != nil {
			_ = rd.Leave()
			return &DecodeError{Stage: "walk", Cause: err}
		}
		if err := fn(h); err != nil {
			_ = rd.Leave()
			return err
		}
	}
}

// skipOrEnter drains a leaf or opens a master for the caller.
func skipOrEnter(rd *ebml.Reader, h ebml.Header) error {
	if ebml.IsMaster(h.ID) {
		return walkChildren(rd, h, func(ch ebml.Header) error {
			return skipOrEnter(rd, ch)
		})
	}
	if err := rd.Skip(h); err != nil {
		return &DecodeError{Stage: "skip", Cause: err}
	}
	return nil
}
