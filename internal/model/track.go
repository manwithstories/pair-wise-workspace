package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// TrackKind is the coarse media class of a track.
type TrackKind string

const (
	KindVideo TrackKind = "video"
	KindAudio TrackKind = "audio"
	KindOther TrackKind = "other"
)

// DisplayMeta is everything a downstream editing system needs to interpret a
// track's samples geometrically and photometrically.
type DisplayMeta struct {
	Rotation int // degrees clockwise, one of 0, 90, 180, 270
	Crop     Crop
	Color    ColorDesc
	Width    int
	Height   int
}

type Crop struct {
	Valid      bool
	X, Y, W, H int
}

func (c Crop) Right() int  { return c.X + c.W }
func (c Crop) Bottom() int { return c.Y + c.H }

// Validate reports whether the crop rectangle lies fully inside the frame.
func (c Crop) Validate(width, height int) error {
	if !c.Valid {
		return nil
	}
	if c.W <= 0 || c.H <= 0 {
		return NewError(CodeCropRange, "crop size must be positive: %dx%d", c.W, c.H)
	}
	if c.X < 0 || c.Y < 0 || c.Right() > width || c.Bottom() > height {
		return NewError(CodeCropRange,
			"crop %dx%d+%d+%d exceeds %dx%d frame", c.W, c.H, c.X, c.Y, width, height)
	}
	return nil
}

// ColorDesc is a normalized colour description. Unknown tokens are preserved
// as "unknown" rather than being guessed at.
type ColorDesc struct {
	Primaries   string
	Transfer    string
	Matrix      string
	FullRange   string // "full", "limited" or "unknown"
	OriginalTag string // raw container token for audit
}

// UnknownColor is the conservative default when no recognizable description
// exists. It never authorizes rewriting samples.
func UnknownColor(tag string) ColorDesc {
	return ColorDesc{Primaries: "unknown", Transfer: "unknown", Matrix: "unknown", FullRange: "unknown", OriginalTag: tag}
}

// Sample is one coded media sample as it appeared in the source container.
type Sample struct {
	Index        int
	DTS          int64 // in the track timescale
	PTS          int64
	Duration     int64
	Size         int32
	Hash         [32]byte
	SourceOffset int64
}

func (s Sample) HashHex() string { return hex.EncodeToString(s.Hash[:]) }

// Track is a demultiplexed elementary stream.
type Track struct {
	ID                string
	Kind              TrackKind
	Timescale         int64
	SampleDesc        string
	Samples           []Sample
	Display           DisplayMeta
	ContainerColor    ColorDesc
	ContainerRotation int
	ContainerCrop     Crop
	TrackColor        ColorDesc // per-track descriptor (e.g. MXF essence)
	TrackRotation     int
	TrackCrop         Crop
	CodedWidth        int
	CodedHeight       int
	SourcePath        string
}

// Fingerprint derives a stable per-track identity from content, not from the
// order in which the file happened to be listed. It is therefore safe to use
// as a mapping key even when inputs are reordered.
func (t *Track) Fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "kind=%s;timescale=%d;desc=%s;n=%d;", t.Kind, t.Timescale, t.SampleDesc, len(t.Samples))
	// Hash of ordered sample hashes — order inside a track is meaningful.
	for _, s := range t.Samples {
		h.Write(s.Hash[:])
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// TrackDuration returns the span of calibrated sample presentation times.
func (t *Track) DurationTicks() int64 {
	var maxPTS int64
	for _, s := range t.Samples {
		if end := s.PTS + s.Duration; end > maxPTS {
			maxPTS = end
		}
	}
	return maxPTS
}

// Event is the unified, calibrated model shared by the mapper and the
// timeline engine. Times are nanoseconds on the unified timeline.
type Event struct {
	TrackID      string
	Fingerprint  string
	Kind         TrackKind
	Index        int
	Seq          uint32 // transport sequence number, when known
	DTS          Instant
	PTS          Instant
	Duration     Instant
	Hash         [32]byte
	Size         int32
	SourceOffset int64
	IsGap        bool // explicit gap marker, carries no sample bytes
	Segment      int  // closed-interval segment assigned by the watermark
	Arrival      int  // receive order within a source; drives late detection
}

func (e Event) HasHash() bool { return !e.IsGap }

func (e Event) HashHex() string { return hex.EncodeToString(e.Hash[:]) }
