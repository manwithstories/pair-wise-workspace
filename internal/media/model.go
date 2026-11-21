package media

import (
	"fmt"
	"io"
	"sort"
)

// MediaType is the coarse classification used for track mapping. Mapping is
// keyed by stable track identity and media type; it never merges tracks.
type MediaType string

const (
	MediaVideo MediaType = "video"
	MediaAudio MediaType = "audio"
	MediaOther MediaType = "other"
)

// TrackID is the stable identity of a track: the container format plus the
// identifier stored in that container. Two tracks with the same TrackID in
// one container are a hard error, even if their media types differ.
type TrackID struct {
	Container string
	Value     uint32
}

func (id TrackID) String() string {
	return fmt.Sprintf("%s:%d", id.Container, id.Value)
}

// Source is the random-access input shared by adapters and the muxer. It is
// deliberately not an io.Reader: streaming is done by bounded ReadAt calls
// so a 1 GiB logical stream never has to be resident in memory.
type Source interface {
	io.ReaderAt
	Size() int64
	Name() string
}

// Sample is the container-independent description of one coded sample.
// Data is expressed as a byte range in the source; the pipeline reads it in
// bounded chunks. Payload bytes are never copied into this structure.
type Sample struct {
	Seq      uint32 // 0-based ordinal within the track
	DTS      Rat    // decode timestamp, seconds
	PTS      Rat    // presentation timestamp, seconds
	Duration Rat    // sample duration, seconds
	Offset   int64  // absolute byte offset in the source
	Size     int64
	Sync     bool
}

// SampleIndex exposes the sample table of a track without materializing a
// per-sample struct for large inputs. Implementations must be read-only and
// safe for sequential use.
type SampleIndex interface {
	Count() int
	At(i int) (Sample, error)
}

// SliceIndex is a simple in-memory index, used by the generated MXF subset
// and by tests.
type SliceIndex []Sample

func (s SliceIndex) Count() int { return len(s) }

func (s SliceIndex) At(i int) (Sample, error) {
	if i < 0 || i >= len(s) {
		return Sample{}, NewError(ClassMalformed, "sample index %d out of range [0,%d)", i, len(s))
	}
	return s[i], nil
}

// Color is a colour description taken from a colr-style element. Present is
// false when the source said nothing; the resolver then records the
// documented default while preserving the conflict source.
type Color struct {
	Present   bool
	Primaries uint16
	Transfer  uint16
	Matrix    uint16
	FullRange bool
	Source    string // "container", "track", "override"
}

// DefaultColor is the documented fallback, matching BT.709 limited range.
var DefaultColor = Color{
	Primaries: 1, // BT.709
	Transfer:  1,
	Matrix:    1,
	FullRange: false,
	Source:    "default",
}

func (c Color) String() string {
	return fmt.Sprintf("p=%d,t=%d,m=%d,full=%t,src=%s", c.Primaries, c.Transfer, c.Matrix, c.FullRange, c.Source)
}

// Crop is a clean-aperture style rectangle in coded pixels. All fields are
// non-negative and describe the visible picture inside the coded frame.
type Crop struct {
	X      int
	Y      int
	Width  int
	Height int
	Source string
}

func (c Crop) String() string {
	return fmt.Sprintf("%dx%d+%d+%d,src=%s", c.Width, c.Height, c.X, c.Y, c.Source)
}

// Track is the unified model produced by every adapter. Adapters fill the
// descriptive fields and a SampleIndex; the pipeline only reads samples
// through the index.
type Track struct {
	ID         TrackID
	Type       MediaType
	Codec      string
	Timescale  uint32
	Duration   Rat
	Width      int // coded width in pixels, video only
	Height     int
	Language   string
	Index      SampleIndex
	Color      Color // track-level description, may be absent
	Rotation   int   // quarter turns, counter-clockwise, 0..3
	HasRotate  bool
	Crop       *Crop
	SourcePath string
}

// SampleCount returns the number of coded samples in the track.
func (t *Track) SampleCount() int {
	if t.Index == nil {
		return 0
	}
	return t.Index.Count()
}

// Container is the parsed description of one input file.
type Container struct {
	Format string // "mp4" or "mxf"
	Path   string
	Size   int64
	Tracks []*Track
	// Rotation and Color captured at container level, before track-level
	// values are considered.
	Rotation  int
	HasRotate bool
	Color     Color
	// UnknownBoxes counts skipped boxes or KLVs.
	UnknownBoxes int
	// Fingerprint identifies the input for checkpoint validation.
	Fingerprint string
}

// SortTracks orders tracks deterministically by media type, then stable ID.
// Mapping output is required to be reproducible, so every consumer uses
// this order rather than map iteration.
func SortTracks(tracks []*Track) {
	sort.SliceStable(tracks, func(i, j int) bool {
		a, b := tracks[i], tracks[j]
		if a.ID.Container != b.ID.Container {
			return a.ID.Container < b.ID.Container
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.ID.Value < b.ID.Value
	})
}

// ValidateIdentity rejects missing and duplicate stable identifiers. It runs
// before mapping so a duplicate can never be silently merged downstream.
func ValidateIdentity(tracks []*Track) error {
	seen := make(map[TrackID]MediaType, len(tracks))
	for _, t := range tracks {
		if t.ID.Container == "" {
			return NewError(ClassMissingTrackID, "track %d has no container identity", t.ID.Value)
		}
		if t.ID.Value == 0 {
			return NewError(ClassMissingTrackID, "track identifier must be non-zero").WithContainer(t.ID.Container)
		}
		if prev, ok := seen[t.ID]; ok {
			return NewError(ClassDuplicateTrackID,
				"identifier %s is claimed by both %s and %s tracks", t.ID, prev, t.Type).
				WithContainer(t.ID.Container).
				WithTrack(t.ID.Value)
		}
		seen[t.ID] = t.Type
	}
	return nil
}
