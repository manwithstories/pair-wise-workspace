// Package model defines the shared track, sample and error vocabulary used by
// every mediasync subsystem.
package model

import (
	"errors"
	"fmt"
)

// TrackKind classifies media essence.
type TrackKind string

const (
	KindVideo TrackKind = "video"
	KindAudio TrackKind = "audio"
	KindData  TrackKind = "data"
)

// Valid reports whether the kind is one of the defined essences.
func (k TrackKind) Valid() bool {
	switch k {
	case KindVideo, KindAudio, KindData:
		return true
	}
	return false
}

// Source names where a track or metadata claim came from.
type Source string

const (
	SourceMP4      Source = "mp4"
	SourceMXF      Source = "mxf"
	SourceRTP      Source = "rtp"
	SourceTrack    Source = "track"
	SourceExplicit Source = "explicit"
	SourceDefault  Source = "default"
)

// Sample is a single coded sample. Sizes are byte counts of the coded payload;
// mediasync never re-encodes, so a sample's identity is (size, offset, time).
type Sample struct {
	// Index is the zero-based position in decode order within its track.
	Index int
	// Offset is the byte offset of the sample payload within the source input.
	Offset int64
	// Size is the coded payload length in bytes.
	Size int
	// DTS and PTS are in the track's Timescale ticks.
	DTS int64
	PTS int64
	// Duration is the sample duration in track ticks.
	Duration int64
	// Marker is set for the final sample of a RTP-depacketised block.
	Marker bool
}

// Track is the common representation emitted by the MP4 and MXF readers and
// consumed by mapping, normalisation, checkpointing and remuxing.
type Track struct {
	Source    Source
	ID        uint32
	Kind      TrackKind
	Codec     string
	Timescale uint32
	Samples   []Sample

	// Codec-private description bytes (e.g. avcC). Copied verbatim.
	CodecConfig []byte

	Width, Height int
	SampleRate    int
	Channels      int

	// Meta holds container-level display metadata reported for this track.
	Meta DisplayMeta
}

// ByteCount sums coded sample bytes.
func (t *Track) ByteCount() int64 {
	var n int64
	for _, s := range t.Samples {
		n += int64(s.Size)
	}
	return n
}

// SampleCount returns the number of coded samples.
func (t *Track) SampleCount() int { return len(t.Samples) }

// DurationTicks returns the end time of the last sample in track ticks.
func (t *Track) DurationTicks() int64 {
	if len(t.Samples) == 0 {
		return 0
	}
	last := t.Samples[len(t.Samples)-1]
	return last.DTS + last.Duration
}

// MediaDuration is the track duration in seconds as a rational via Ticks fields.
func (t *Track) MediaDurationNum() (num, den int64) {
	if t.Timescale == 0 {
		return 0, 1
	}
	return t.DurationTicks(), int64(t.Timescale)
}

// TrackStats is the per-track accounting a reader or remuxer reports.
type TrackStats struct {
	TrackID uint32     `json:"track_id"`
	Kind    TrackKind  `json:"kind"`
	Samples int        `json:"samples"`
	Bytes   int64      `json:"bytes"`
	SHA256  [32]byte   `json:"-"`
}

// Crop is a display crop in pixels removed from each source edge.
type Crop struct {
	Left, Right, Top, Bottom int
}

// IsZero reports whether no pixels are cropped.
func (c Crop) IsZero() bool {
	return c.Left == 0 && c.Right == 0 && c.Top == 0 && c.Bottom == 0
}

// Colour is a colour description. Known distinguishes a recognised, non-default
// value from an unknown one that must be preserved verbatim.
type Colour struct {
	Primaries uint16
	Transfer  uint16
	Matrix    uint16
	FullRange bool
	// Known is false when the description used unrecognised codes; the raw
	// values are still preserved and DefaultInterpretation is recorded.
	Known                 bool
	DefaultInterpretation string
}

// MetaLayer records how strongly a metadata claim is asserted.
type MetaLayer int

const (
	LayerContainer MetaLayer = iota // weakest
	LayerTrack
	LayerExplicit // strongest
)

func (l MetaLayer) String() string {
	switch l {
	case LayerContainer:
		return "container"
	case LayerTrack:
		return "track"
	case LayerExplicit:
		return "explicit"
	default:
		return "unknown"
	}
}

// Claim is a single metadata assertion from one layer. Claims are recorded even
// when they lose arbitration so conflict provenance is auditable.
type Claim struct {
	Field  string
	Value  string
	Layer  MetaLayer
	Source Source
	// Reason explains rejection when this claim lost arbitration.
	Reason string
}

// DisplayMeta carries every display-related claim plus the resolved result.
type DisplayMeta struct {
	Rotation    int
	RotationSrc Source
	Crop        Crop
	CropSrc     Source
	Colour      *Colour
	ColourSrc   Source
	// Claims preserves the full arbitration history in arrival order.
	Claims []Claim
	// Warnings are non-fatal notes (unknown colour, default rotation, ...).
	Warnings []string
	// SourceWidth and SourceHeight are the coded dimensions before rotation.
	SourceWidth, SourceHeight int
}

// Clone returns a deep copy safe to mutate during normalisation.
func (m DisplayMeta) Clone() DisplayMeta {
	out := m
	out.Claims = append([]Claim(nil), m.Claims...)
	out.Warnings = append([]string(nil), m.Warnings...)
	if m.Colour != nil {
		c := *m.Colour
		out.Colour = &c
	}
	return out
}

// ErrorCode is a stable machine-readable failure classification.
type ErrorCode string

const (
	ErrTruncatedBox      ErrorCode = "TRUNCATED_BOX"
	ErrInvalidKLV        ErrorCode = "INVALID_KLV"
	ErrTrackMapDuplicate ErrorCode = "TRACK_MAP_DUPLICATE"
	ErrTrackMapMissing   ErrorCode = "TRACK_MAP_MISSING"
	ErrTrackMapMismatch  ErrorCode = "TRACK_MAP_TYPE_MISMATCH"
	ErrCropOutOfBounds   ErrorCode = "CROP_OUT_OF_BOUNDS"
	ErrRTPGap            ErrorCode = "RTP_GAP"
	ErrCheckpointCorrupt ErrorCode = "CHECKPOINT_CORRUPT"
	ErrPathEscape        ErrorCode = "PATH_ESCAPE"
	ErrSizeLimit         ErrorCode = "SIZE_LIMIT"
	ErrParamInvalid      ErrorCode = "PARAM_INVALID"
	ErrUnrecoverable     ErrorCode = "UNRECOVERABLE"
)

// Exit codes agreed with the demo contract.
const (
	ExitOK            = 0
	ExitParamError    = 1
	ExitDataError     = 2
	ExitUnrecoverable = 3
)

// ExitCode maps an error to the process exit status.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var ce *CodedError
	if errors.As(err, &ce) {
		switch ce.Code {
		case ErrParamInvalid, ErrPathEscape, ErrSizeLimit:
			return ExitParamError
		case ErrUnrecoverable, ErrCheckpointCorrupt:
			return ExitUnrecoverable
		default:
			return ExitDataError
		}
	}
	return ExitDataError
}

// CodedError carries classification, the originating track and byte offset so
// terminal output can point an engineer at the exact failure.
type CodedError struct {
	Code    ErrorCode
	Track   uint32
	Offset  int64
	Message string
	Err     error
}

func (e *CodedError) Error() string {
	msg := e.Message
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	if msg == "" {
		msg = string(e.Code)
	}
	if e.Track != 0 {
		return fmt.Sprintf("%s: track %d offset %d: %s", e.Code, e.Track, e.Offset, msg)
	}
	if e.Offset != 0 {
		return fmt.Sprintf("%s: offset %d: %s", e.Code, e.Offset, msg)
	}
	return fmt.Sprintf("%s: %s", e.Code, msg)
}

func (e *CodedError) Unwrap() error { return e.Err }

// CodeOf extracts the error code, defaulting to ErrUnrecoverable.
func CodeOf(err error) ErrorCode {
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ErrUnrecoverable
}

// NewError builds a coded error.
func NewError(code ErrorCode, track uint32, offset int64, format string, args ...any) *CodedError {
	return &CodedError{Code: code, Track: track, Offset: offset, Message: fmt.Sprintf(format, args...)}
}

// WrapError attaches a code to an underlying error.
func WrapError(code ErrorCode, track uint32, offset int64, err error) *CodedError {
	return &CodedError{Code: code, Track: track, Offset: offset, Err: err}
}
