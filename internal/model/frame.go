// Package model defines the shared data types and typed errors used by
// every medpipe subsystem.
package model

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// Kind identifies the media kind carried by a track.
type Kind uint8

const (
	KindVideo Kind = 1
	KindAudio Kind = 2
)

func (k Kind) String() string {
	switch k {
	case KindVideo:
		return "video"
	case KindAudio:
		return "audio"
	default:
		return fmt.Sprintf("kind(%d)", uint8(k))
	}
}

// Track describes one capture source. ClockBits is the width of the raw
// capture counter (16 or 32 in practice). NominalTickDelta is the expected
// number of counter ticks between frames and is used for drift estimation.
type Track struct {
	ID               string    `json:"id"`
	Kind             Kind      `json:"kind"`
	Timescale        uint32    `json:"timescale"`
	SampleRate       uint32    `json:"sample_rate,omitempty"`
	ClockBits        uint8     `json:"clock_bits"`
	NominalTickDelta uint64    `json:"nominal_tick_delta"`
	OriginNS         int64     `json:"origin_ns"`
	Encapsulation    string    `json:"encapsulation,omitempty"`
	Extra            []Metadata `json:"extra,omitempty"`
}

// Metadata is an ordered key/value annotation. Ordering keeps canonical
// serialization deterministic.
type Metadata struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Frame is one decoded unit of a track. Payload holds the encoded content;
// Energy is only meaningful for audio tracks and contains per-frame block
// energies (0..1). Meta is non-content annotation and never contributes to
// content fingerprints.
type Frame struct {
	TrackID  string
	Seq      uint64
	RawTS    uint64
	Keyframe bool
	Width    uint16
	Height   uint16
	Energy   []float64
	Payload  []byte
	Checksum [32]byte
	Meta     []Metadata
	Offset   int64
}

const (
	integrityDomain = "medpipe/integrity/v1\x00"
	contentDomain   = "medpipe/content/v1\x00"
)

// IntegrityChecksum binds the track, frame number, raw timestamp and payload.
// Consumer-corrected metadata never changes it.
func (f *Frame) IntegrityChecksum() [32]byte {
	h := sha256.New()
	h.Write([]byte(integrityDomain))
	h.Write([]byte(f.TrackID))
	h.Write([]byte{0})
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], f.Seq)
	h.Write(b[:])
	binary.BigEndian.PutUint64(b[:], f.RawTS)
	h.Write(b[:])
	h.Write(f.Payload)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ContentDigest covers only identity and payload. Changing Width, Height,
// Energy, Keyframe flags or Meta must not alter it; changing payload must.
func (f *Frame) ContentDigest() [32]byte {
	h := sha256.New()
	h.Write([]byte(contentDomain))
	h.Write([]byte(f.TrackID))
	h.Write([]byte{0})
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], f.Seq)
	h.Write(b[:])
	h.Write(f.Payload)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ErrorKind classifies failures so the CLI can return stable exit codes.
type ErrorKind int

const (
	KindInput ErrorKind = iota + 1
	KindCorrupt
	KindAmbiguous
	KindNotFound
	KindInvariant
)

func (k ErrorKind) String() string {
	switch k {
	case KindInput:
		return "input"
	case KindCorrupt:
		return "corrupt"
	case KindAmbiguous:
		return "ambiguous"
	case KindNotFound:
		return "not_found"
	case KindInvariant:
		return "invariant"
	default:
		return "unknown"
	}
}

// Error is the single error type shared by all packages. TrackID, Seq and
// Offset are filled whenever the failure can be attributed to a frame.
type Error struct {
	Kind    ErrorKind
	Op      string
	TrackID string
	Seq     uint64
	Offset  int64
	Msg     string
	Err     error
}

func (e *Error) Error() string {
	msg := e.Msg
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	loc := ""
	if e.TrackID != "" {
		loc = fmt.Sprintf(" track=%s frame=%d", e.TrackID, e.Seq)
	}
	if e.Offset != 0 {
		loc += fmt.Sprintf(" offset=%d", e.Offset)
	}
	return fmt.Sprintf("%s: %s%s: %s", e.Kind, e.Op, loc, msg)
}

func (e *Error) Unwrap() error { return e.Err }

func New(kind ErrorKind, op, msg string) *Error {
	return &Error{Kind: kind, Op: op, Msg: msg}
}

func Wrap(kind ErrorKind, op string, err error) *Error {
	return &Error{Kind: kind, Op: op, Err: err}
}

func Corruption(op, track string, seq uint64, offset int64, msg string) *Error {
	return &Error{Kind: KindCorrupt, Op: op, TrackID: track, Seq: seq, Offset: offset, Msg: msg}
}

// ExitCode maps any error to the required process exit status: 0 for nil,
// 2 for bad input or dependency failures, 1 for internal invariant breaks.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var me *Error
	if errors.As(err, &me) {
		switch me.Kind {
		case KindInvariant:
			return 1
		case KindInput, KindCorrupt, KindAmbiguous, KindNotFound:
			return 2
		}
	}
	return 1
}

func IsKind(err error, kind ErrorKind) bool {
	var me *Error
	return errors.As(err, &me) && me.Kind == kind
}
