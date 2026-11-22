// Package container defines the versioned container-adapter contract and
// the in-memory model shared by the MP4 reader, the codec layer and the
// processing engine.
//
// An adapter parses only structure: it never loads a whole track. Samples
// are addressed by absolute byte offset and are streamed through a bounded
// cursor, so the engine can hold at most one sample plus one output block
// at any time.
package container

import (
	"errors"
	"io"

	"mediacore/internal/media"
)

// Version identifies a container adapter revision. Checkpoints are bound to
// this string; changing parser behaviour requires bumping it so that stale
// checkpoints are rejected instead of silently reused.
type Version struct {
	Name  string
	Major int
	Minor int
}

// String renders a stable identifier such as "mp4/1.0".
func (v Version) String() string {
	name := v.Name
	if name == "" {
		name = "unknown"
	}
	return name + "/" + itoa(v.Major) + "." + itoa(v.Minor)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	p := len(buf)
	for i > 0 {
		p--
		buf[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		buf[p] = '-'
	}
	return string(buf[p:])
}

// ErrUnsupportedContainer is returned when no adapter claims the input.
var ErrUnsupportedContainer = errors.New("container: unsupported container")

// Options bounds every allocation made while parsing. The defaults keep the
// adapter far below the 128 MiB process limit even for hostile input.
type Options struct {
	// MaxMetadataBytes caps a single structural box (moov, stbl, ...).
	MaxMetadataBytes int64
	// MaxSampleBytes caps a single decoded sample read.
	MaxSampleBytes int64
	// MaxSamplesPerTrack caps stsz/stts expansion.
	MaxSamplesPerTrack int
	// MaxChunkCount caps stco/co64/stsc expansion.
	MaxChunkCount int
}

// DefaultOptions returns the production bounds.
func DefaultOptions() Options {
	return Options{
		MaxMetadataBytes:   64 << 20,
		MaxSampleBytes:     16 << 20,
		MaxSamplesPerTrack: 20 << 20,
		MaxChunkCount:      8 << 20,
	}
}

// Range is a half-open byte interval [Start, End).
type Range struct {
	Start int64
	End   int64
}

// Contains reports whether r covers n bytes at off.
func (r Range) Contains(off, n int64) bool {
	return off >= r.Start && n >= 0 && off+n <= r.End
}

// Track describes one audio track discovered by an adapter.
type Track struct {
	// Number is the 1-based index among audio tracks; it is the track
	// value reported in errors.
	Number        int
	ID            int
	Timescale     int64
	Duration      int64 // in track ticks, from mdhd when present
	Handler       string
	SampleCount   int
	CodecTag      string // stsd sample-entry fourcc, e.g. "sowt"
	SampleRate    int
	Channels      int
	BitsPerSample int
	// Config is the codec-specific configuration payload (e.g. AAC
	// AudioSpecificConfig extracted from esds).
	Config []byte
	// FirstSampleOffset is the file offset of sample 0, useful for
	// provenance; -1 when there are no samples.
	FirstSampleOffset int64
	// MdatIndex is the index of the mdat range that holds this track's
	// samples.
	MdatIndex int
}

// Info summarises the parsed file.
type Info struct {
	FileSize    int64
	MajorBrand  string
	Compatible  []string
	MoovOffset  int64
	Mdats       []Range
	TrackCount  int
	AudioTracks int
	DurationMS  int64
}

// Sample is one container sample with its position and timing.
type Sample struct {
	Track    int
	Index    int
	Offset   int64
	Size     int
	DTS      int64 // track timescale ticks
	Duration int64 // track timescale ticks
	PTS      int64 // DTS + composition offset
	// Data is populated by the cursor and never exceeds MaxSampleBytes.
	Data []byte
}

// SampleCursor streams samples of a single track. Next returns io.EOF at the
// end of the track. The memory held by a cursor is bounded by one sample.
type SampleCursor interface {
	Next() (Sample, error)
	Close() error
}

// Source is an open, parsed container. It retains only the sample table;
// media bytes are read lazily from the underlying ReaderAt.
type Source struct {
	info    Info
	tracks  []Track
	refs    [][]SampleRef
	mdats   []Range
	r       io.ReaderAt
	opts    Options
	version Version
}

// SampleRef is the parsed location and timing of one sample.
type SampleRef struct {
	Offset   int64
	Size     int32
	DTS      int64
	Duration int64
	CTTSSign int64
	CTTS     int64
}

// Info returns the file summary.
func (s *Source) Info() Info { return s.info }

// Tracks returns all audio tracks.
func (s *Source) Tracks() []Track { return s.tracks }

// Track returns track n (1-based) if present.
func (s *Source) Track(n int) (Track, bool) {
	if n < 1 || n > len(s.tracks) {
		return Track{}, false
	}
	return s.tracks[n-1], true
}

// Reader returns the underlying random-access reader so the engine can
// stream media bytes without copying them through the parser.
func (s *Source) Reader() io.ReaderAt { return s.r }

// CoveringMdat returns the mdat range containing [off, off+n), or false.
func (s *Source) CoveringMdat(off, n int64) (Range, bool) {
	for _, m := range s.mdats {
		if m.Contains(off, n) {
			return m, true
		}
	}
	return Range{}, false
}

// Version returns the adapter version that produced this source.
func (s *Source) Version() Version { return s.version }

// Cursor returns a bounded cursor over track n.
func (s *Source) Cursor(n int) (SampleCursor, error) {
	if n < 1 || n > len(s.refs) {
		return nil, ErrUnsupportedContainer
	}
	return &cursor{src: s, track: n, refs: s.refs[n-1]}, nil
}

type cursor struct {
	src   *Source
	track int
	refs  []SampleRef
	pos   int
	prev  int64 // previous PTS, for regression detection
	start bool
}

func (c *cursor) Close() error { return nil }

func (c *cursor) Next() (Sample, error) {
	if c.pos >= len(c.refs) {
		return Sample{}, io.EOF
	}
	ref := c.refs[c.pos]
	s := Sample{
		Track:    c.track,
		Index:    c.pos,
		Offset:   ref.Offset,
		Size:     int(ref.Size),
		DTS:      ref.DTS,
		Duration: ref.Duration,
		PTS:      ref.DTS + ref.CTTS,
	}
	if c.start && s.PTS < c.prev {
		return Sample{}, media.NewError(media.CodeTimestampRegression, s.Offset, s.Track,
			"sample %d pts %d < previous pts %d", s.Index, s.PTS, c.prev)
	}
	c.prev = s.PTS
	c.start = true
	if s.Size < 0 || int64(s.Size) > c.src.opts.MaxSampleBytes {
		return Sample{}, media.NewError(media.CodeMemoryLimit, s.Offset, s.Track,
			"sample %d size %d exceeds limit %d", s.Index, s.Size, c.src.opts.MaxSampleBytes)
	}
	buf := make([]byte, s.Size)
	if s.Size > 0 {
		if _, err := c.src.r.ReadAt(buf, s.Offset); err != nil {
			return Sample{}, media.NewError(media.CodeTruncatedMdat, s.Offset, s.Track,
				"sample %d read failed: %v", s.Index, err).Wrap(err)
		}
	}
	s.Data = buf
	c.pos++
	return s, nil
}
