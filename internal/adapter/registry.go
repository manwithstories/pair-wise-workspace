package adapter

import (
	"io"

	"avpipe/internal/media"
)

// ParseResult is the output of a frame-container parse.
type ParseResult struct {
	Frames      []media.Frame
	Index       []IndexEntry
	Audio       []AudioHeader
	UsedFallback bool
	Errors      []*media.Error
}

// AudioHeader is one aligned PCM block descriptor. When the parse succeeds
// Block carries the decoded samples; DataOffset locates the raw bytes inside
// the container's audio section for error reporting.
type AudioHeader struct {
	media.AudioBlock
	DataOffset uint64
}

// FrameParser is implemented by every versioned container parser.
type FrameParser interface {
	Version() uint8
	// Parse consumes a length-bounded stream. Truncated tails keep only
	// complete records and are reported through ParseResult.Errors.
	Parse(r io.Reader, size int64) (*ParseResult, error)
}

var registry = map[uint8]func() FrameParser{
	ContainerVersion1: func() FrameParser { return &frameV1Parser{} },
}

// RegisterParser lets additional versions register themselves.
func RegisterParser(version uint8, factory func() FrameParser) {
	registry[version] = factory
}

// LookupParser resolves a parser by version; an unknown version is an
// explicit, locatable failure rather than a silent fallback.
func LookupParser(version uint8) (FrameParser, error) {
	factory, ok := registry[version]
	if !ok {
		return nil, media.NewError("parser", media.KindUnknownVersion, 4,
			media.ActionFail, "container version %d is not supported", version)
	}
	return factory(), nil
}

// ParseContainer dispatches to the parser named by the version byte of the
// header. The header itself is read here so unknown versions fail fast.
func ParseContainer(r io.Reader, size int64) (*ParseResult, error) {
	if size <= 0 || size > MaxInputSize {
		return nil, media.NewError("parser", media.KindLimitExceeded, 0,
			media.ActionFail, "input size %d out of allowed range (0, %d]", size, MaxInputSize)
	}
	var hdr [HeaderSize]byte
	if err := readFull(r, 0, hdr[:], size); err != nil {
		return nil, media.NewError("parser", media.KindTruncated, 0,
			media.ActionFail, "header incomplete: %v", err)
	}
	var magic [4]byte
	copy(magic[:], hdr[0:4])
	if magic != MagicHeader {
		return nil, media.NewError("parser", "bad_magic", 0,
			media.ActionFail, "not an AVPI container")
	}
	version := hdr[4]
	parser, err := LookupParser(version)
	if err != nil {
		return nil, err
	}
	// Re-seek equivalent: wrap so the version parser sees the full stream.
	seeker, ok := r.(io.ReadSeeker)
	if !ok {
		return nil, media.NewError("parser", "io_error", 0,
			media.ActionFail, "parser requires an io.ReadSeeker")
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return parser.Parse(seeker, size)
}
