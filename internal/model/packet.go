// Package model defines the packet identity, event time and error taxonomy
// shared by the ingest, timeline and delivery subsystems. No subsystem may
// invent its own time unit or duplicate classification.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Codec identifies the audio coding of an event.
type Codec string

const (
	CodecOpus Codec = "opus"
	CodecAAC  Codec = "aac"
)

// Valid reports whether the codec is supported.
func (c Codec) Valid() bool { return c == CodecOpus || c == CodecAAC }

// ErrorKind is the shared failure classification. Both the pipeline metrics
// and the quarantine records use these values verbatim.
type ErrorKind string

const (
	KindNone          ErrorKind = ""
	KindCorrupt       ErrorKind = "corrupt-payload"    // payload failed validation; duration unprovable
	KindInvalidPacket ErrorKind = "invalid-packet"     // header/model invariant broken
	KindClockJump     ErrorKind = "illegal-clock-jump" // event time outside jump limits
	KindOversize      ErrorKind = "payload-oversize"   // payload above 1 MiB
	KindLate          ErrorKind = "late-beyond-window" // event arrived after watermark passed
	KindUnknownGap    ErrorKind = "unknown-gap"        // gap with no recovery evidence
)

// AllErrorKinds lists every quarantine reason the pipeline can emit.
var AllErrorKinds = []ErrorKind{
	KindCorrupt, KindInvalidPacket, KindClockJump, KindOversize, KindLate, KindUnknownGap,
}

// ClassifiedError wraps an error with the shared kind.
type ClassifiedError struct {
	Kind ErrorKind
	Err  error
}

func (e *ClassifiedError) Error() string {
	if e.Err == nil {
		return string(e.Kind)
	}
	return string(e.Kind) + ": " + e.Err.Error()
}

func (e *ClassifiedError) Unwrap() error { return e.Err }

// Classify attaches kind to err. It preserves an existing classification.
func Classify(kind ErrorKind, err error) error {
	if err == nil {
		err = errors.New(string(kind))
	}
	var ce *ClassifiedError
	if errors.As(err, &ce) {
		return err
	}
	return &ClassifiedError{Kind: kind, Err: err}
}

// KindOf extracts the classification, or KindNone.
func KindOf(err error) ErrorKind {
	var ce *ClassifiedError
	if errors.As(err, &ce) {
		return ce.Kind
	}
	return KindNone
}

// MaxPayloadBytes is the hard per-packet payload limit (1 MiB).
const MaxPayloadBytes = 1 << 20

// StreamID names one RTP stream (one SSRC plus one representation).
type StreamID string

// RepresentationID names one bitrate representation of the programme.
type RepresentationID string

// Packet identity. Identity is stream + sequence number. Payload content is
// deliberately not part of identity so a retransmission of the same sequence
// with different bytes is still detectable as a conflict.
type PacketID struct {
	Stream StreamID
	Seq    uint32 // extended 16-bit sequence number
}

func (p PacketID) String() string { return fmt.Sprintf("%s/%d", p.Stream, p.Seq) }

// Event is one audio packet event after ingest normalisation.
type Event struct {
	ID         PacketID
	Codec      Codec
	RTPTime    uint32
	ExtTimeUS  int64  // unified event time on the first-packet base
	DurationUS int64  // -1 when the duration cannot be inferred (corrupt)
	Payload    []byte // may be nil for a declared-only packet
	Marker     bool
	// ArrivalUS is the wall-clock arrival offset; it drives the lateness
	// watermark but never the media timeline.
	ArrivalUS int64
	// ExplainedBy links a recovered event to the packet that filled it.
	ExplainedBy *PacketID
}

// PayloadHash is the SHA-256 of the payload (empty payload hashes the empty
// string). It is used for idempotent duplicate detection.
func (e Event) PayloadHash() string {
	sum := sha256.Sum256(e.Payload)
	return hex.EncodeToString(sum[:])
}

// TimeRange is a half-open [Start, End) interval in microseconds.
type TimeRange struct {
	Start int64
	End   int64
}

// Duration returns the interval length.
func (r TimeRange) Duration() int64 { return r.End - r.Start }

// Contains reports whether t is in [Start, End).
func (r TimeRange) Contains(t int64) bool { return t >= r.Start && t < r.End }

// Overlaps reports whether two intervals share any time.
func (r TimeRange) Overlaps(o TimeRange) bool { return r.Start < o.End && o.Start < r.End }

// SegmentID is the deterministic identifier of a delivery segment.
type SegmentID struct {
	Rep     RepresentationID
	Index   int
	StartUS int64
	EndUS   int64
}

func (s SegmentID) String() string {
	return fmt.Sprintf("%s/%05d/%d-%d", s.Rep, s.Index, s.StartUS, s.EndUS)
}

// Representation is one bitrate of the ladder.
type Representation struct {
	ID        RepresentationID
	Bitrate   int
	Codec     Codec
	ClockRate uint32
	// FrameDurationUS is the representative packet duration for this
	// representation; segment boundaries are aligned on this grid.
	FrameDurationUS int64
	EdgeTrimUS      int64 // actual deviation accepted at the final boundary
}

// Segment is one aligned output slice.
type Segment struct {
	Rep          RepresentationID
	Index        int
	StartUS      int64
	EndUS        int64
	FrameCount   int
	SourceStart  int64 // earliest source packet event time overlapping
	SourceEnd    int64 // latest source packet end overlapping
	BoundarySkew int64 // |actual source boundary - grid boundary|
	OutputSHA256 string
	Version      int
}

// Quarantine records an event that could not be placed on the stable timeline.
type Quarantine struct {
	ID         PacketID  `json:"id"`
	Kind       ErrorKind `json:"kind"`
	Detail     string    `json:"detail"`
	RTPTime    uint32    `json:"rtp_time"`
	ArrivalUS  int64     `json:"arrival_us"`
	Seq        uint32    `json:"seq"`
	Stream     StreamID  `json:"stream"`
	AtUS       int64     `json:"at_us"`
	PayloadSHA string    `json:"payload_sha256"`
}

// SysNow is injectable for deterministic tests and demos.
type SysNow func() time.Time
