// Package codec contains versioned, bounded H.264 and AAC payload parsers.
package codec

import (
	"mediad/internal/mediaerr"
)

// Codec identifies a supported elementary codec.
type Codec string

const (
	CodecH264 Codec = "h264"
	CodecAAC  Codec = "aac"
)

// Capabilities describes a negotiated parser's supported features.
type Capabilities struct {
	Codec             Codec
	ParserVersion     int
	Input             string
	KeyframeDetection bool
	ADTS              bool
}

// ParseContext carries source location and unwrapped media timestamps.
type ParseContext struct {
	PID    uint16
	Offset int64
	PTS    int64
	DTS    int64
}

// Sample is one parsed access unit or audio frame.
type Sample struct {
	Codec      Codec
	PTS        int64
	DTS        int64
	Keyframe   bool
	Payload    []byte
	NALTypes   []uint8
	SampleRate int
	Channels   int
}

// ParserV1 is the stable version-1 parser negotiation interface. Future
// incompatible behavior should be introduced as ParserV2, not by changing V1.
type ParserV1 interface {
	Capabilities() Capabilities
	Parse(payload []byte, ctx ParseContext) ([]Sample, error)
}

// Registry negotiates explicit parser versions.
type Registry struct {
	parsers map[Codec]map[int]ParserV1
}

// NewRegistry creates an empty parser registry.
func NewRegistry() *Registry {
	return &Registry{parsers: make(map[Codec]map[int]ParserV1)}
}

// NewDefaultRegistry registers the built-in H.264 and AAC V1 parsers.
func NewDefaultRegistry() *Registry {
	r := NewRegistry()
	r.MustRegister(NewH264ParserV1())
	r.MustRegister(NewAACParserV1())
	return r
}

// Register adds a parser.
func (r *Registry) Register(p ParserV1) error {
	c := p.Capabilities()
	if c.ParserVersion != 1 {
		return mediaerr.New(mediaerr.KindUnknownVersion, -1, 0, false,
			"unsupported parser version %d for %s", c.ParserVersion, c.Codec)
	}
	if r.parsers[c.Codec] == nil {
		r.parsers[c.Codec] = map[int]ParserV1{}
	}
	r.parsers[c.Codec][c.ParserVersion] = p
	return nil
}

// MustRegister panics only for programmer error during built-in setup.
func (r *Registry) MustRegister(p ParserV1) {
	if err := r.Register(p); err != nil {
		panic(err)
	}
}

// Negotiate selects a codec parser at an explicit version.
func (r *Registry) Negotiate(codec Codec, version int) (ParserV1, error) {
	versions, ok := r.parsers[codec]
	if !ok {
		return nil, mediaerr.New(mediaerr.KindUnsupportedCodec, -1, 0, false,
			"unsupported codec %q", codec)
	}
	p, ok := versions[version]
	if !ok {
		return nil, mediaerr.New(mediaerr.KindUnknownVersion, -1, 0, false,
			"unknown %s parser version v%d", codec, version)
	}
	return p, nil
}

// FromStreamType maps MPEG-TS elementary stream types to codec identifiers.
func FromStreamType(streamType uint8) (Codec, bool) {
	switch streamType {
	case 0x1b:
		return CodecH264, true
	case 0x0f, 0x11:
		return CodecAAC, true
	default:
		return "", false
	}
}

func corrupt(ctx ParseContext, format string, args ...any) error {
	return mediaerr.New(mediaerr.KindCorruptPayload, ctx.Offset, ctx.PID, true, format, args...)
}

func unsupported(ctx ParseContext, format string, args ...any) error {
	return mediaerr.New(mediaerr.KindUnsupportedCodec, ctx.Offset, ctx.PID, false, format, args...)
}
