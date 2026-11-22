// Package adapter converts raw per-track input (RTP, fMP4/emsg, SEI) into
// unified events on the shared timeline. Parsers are registered by name and
// must match the supported interface major version; incompatible or unknown
// parsers are rejected with offset and reason.
package adapter

import (
	"fmt"

	"mediaroute/internal/fault"
)

// Kind classifies unified events.
type Kind string

const (
	KindMedia    Kind = "media"
	KindSubtitle Kind = "subtitle"
	KindChapter  Kind = "chapter"
	KindID3      Kind = "id3"
	KindEmsg     Kind = "emsg"
	KindSEI      Kind = "sei"
)

// SupportedMajor is the parser interface major version this build accepts.
const SupportedMajor = 1

// Input limits: every buffer is bounded.
const (
	MaxPacketSize = 64 << 10  // 64 KiB per input unit
	MaxInputBytes = 64 << 20  // 64 MiB per track stream
	MaxTracks     = 64        // per pipeline run
)

// Event is the unified cross-track record on the nanosecond timeline.
type Event struct {
	Track       string
	Kind        Kind
	Seq         uint32
	TimestampNs int64
	DurationNs  int64
	Keyframe    bool
	Rendition   string
	Payload     []byte
}

// Parser converts raw track input into unified events.
type Parser interface {
	// MajorVersion reports the parser's interface major version.
	MajorVersion() int
	// Parse converts one input unit; offset is its byte offset within the
	// track stream, used when reporting faults.
	Parse(track string, data []byte, offset int64) ([]Event, error)
}

// Registry enforces major-version compatibility across named parsers.
type Registry struct {
	parsers map[string]Parser
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{parsers: map[string]Parser{}} }

// Register adds a parser under a name; later registrations replace earlier.
func (r *Registry) Register(name string, p Parser) { r.parsers[name] = p }

// Parse dispatches to a registered parser after a major-version check.
// Unknown parsers and incompatible major versions are rejected as
// classified version faults carrying the input offset and reason.
func (r *Registry) Parse(name, track string, data []byte, offset int64) ([]Event, error) {
	if len(data) > MaxPacketSize {
		return nil, fault.Newf(fault.CatParse, "input unit %d bytes exceeds limit %d",
			len(data), MaxPacketSize).WithTrack(track).WithOffset(offset)
	}
	p, ok := r.parsers[name]
	if !ok {
		return nil, fault.Newf(fault.CatVersion, "unknown parser %q", name).
			WithTrack(track).WithOffset(offset)
	}
	if v := p.MajorVersion(); v != SupportedMajor {
		return nil, fault.Newf(fault.CatVersion,
			"parser %q major version %d incompatible with supported %d", name, v, SupportedMajor).
			WithTrack(track).WithOffset(offset)
	}
	events, err := p.Parse(track, data, offset)
	if err != nil {
		return nil, fmt.Errorf("parser %s: %w", name, err)
	}
	return events, nil
}
