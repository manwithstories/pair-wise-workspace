package ebml

import (
	"fmt"
	"sort"

	"avcheck/internal/media"
)

// Hard caps. User supplied limits may lower these but never exceed them.
const (
	HardMaxElementSize = 64 << 20 // 64 MiB
	HardMaxBlockSize   = 8 << 20  // 8 MiB
	HardMaxDepth       = 32
	HardMaxTracks      = 128
	HardMaxBlocks      = 1_000_000
	HardMaxTotalBytes  = 64 << 20 // total block payload retained
	HardMaxReadBuffer  = 8 << 20
	HardMaxDurationNs  = int64(24 * 60 * 60 * 1e9)
)

// Limits bounds every allocation and loop driven by the input. Validate
// rejects values above the hard caps instead of silently clamping them.
type Limits struct {
	MaxElementSize int64
	MaxBlockSize   int64
	MaxDepth       int
	MaxTracks      int
	MaxBlocks      int
	MaxTotalBytes  int64
	ReadBuffer     int
	MaxDurationNs  int64
}

// DefaultLimits returns the conservative defaults used by the CLI.
func DefaultLimits() Limits {
	return Limits{
		MaxElementSize: 16 << 20,
		MaxBlockSize:   4 << 20,
		MaxDepth:       16,
		MaxTracks:      64,
		MaxBlocks:      200_000,
		MaxTotalBytes:  32 << 20,
		ReadBuffer:     64 << 10,
		MaxDurationNs:  6 * 60 * 60 * 1e9,
	}
}

// Validate reports the first limit above a hard cap.
func (l Limits) Validate() error {
	if l.MaxElementSize <= 0 || l.MaxElementSize > HardMaxElementSize {
		return fmt.Errorf("ebml: max element size %d outside (0,%d]", l.MaxElementSize, HardMaxElementSize)
	}
	if l.MaxBlockSize <= 0 || l.MaxBlockSize > HardMaxBlockSize {
		return fmt.Errorf("ebml: max block size %d outside (0,%d]", l.MaxBlockSize, HardMaxBlockSize)
	}
	if l.MaxDepth <= 0 || l.MaxDepth > HardMaxDepth {
		return fmt.Errorf("ebml: max depth %d outside (0,%d]", l.MaxDepth, HardMaxDepth)
	}
	if l.MaxTracks <= 0 || l.MaxTracks > HardMaxTracks {
		return fmt.Errorf("ebml: max tracks %d outside (0,%d]", l.MaxTracks, HardMaxTracks)
	}
	if l.MaxBlocks <= 0 || l.MaxBlocks > HardMaxBlocks {
		return fmt.Errorf("ebml: max blocks %d outside (0,%d]", l.MaxBlocks, HardMaxBlocks)
	}
	if l.MaxTotalBytes <= 0 || l.MaxTotalBytes > HardMaxTotalBytes {
		return fmt.Errorf("ebml: max total bytes %d outside (0,%d]", l.MaxTotalBytes, HardMaxTotalBytes)
	}
	if l.ReadBuffer <= 0 || l.ReadBuffer > HardMaxReadBuffer {
		return fmt.Errorf("ebml: read buffer %d outside (0,%d]", l.ReadBuffer, HardMaxReadBuffer)
	}
	if l.MaxDurationNs <= 0 || l.MaxDurationNs > HardMaxDurationNs {
		return fmt.Errorf("ebml: max duration %d outside (0,%d]", l.MaxDurationNs, HardMaxDurationNs)
	}
	return nil
}

// Block is one decodable media (or cue) frame extracted from a cluster.
// Ticks are source ticks on the document timecode scale; TimeNs is filled by
// Finalize. Data is retained only when the caller asks for payloads.
type Block struct {
	Track      uint64
	Ticks      int64 // absolute source ticks (cluster timecode + relative)
	TimeNs     int64 // filled by Finalize
	DurationNs int64 // 0 when unspecified
	Keyframe   bool
	Data       []byte
	Offset     int64 // file offset of the block element header
}

// Track is a track declaration from the Tracks element.
type Track struct {
	Number            uint64
	UID               uint64
	Kind              media.TrackKind
	CodecID           string
	Name              string
	DefaultDurationNs int64
	DependsOn         []uint64 // track numbers declared via AVCHECK_DEPENDS_ON
}

// Document is the parsed container. Faults is always non-nil.
type Document struct {
	DocType       string
	TimecodeScale int64 // nanoseconds per source tick
	DurationNs    int64 // 0 when not declared
	Tracks        []Track
	Blocks        []Block
	Faults        *media.Faults
	StoppedTracks map[uint64]bool // tracks halted by a local fault
	TotalBytes    int64
	truncated     bool
}

// NewDocument returns an initialized document.
func NewDocument() *Document {
	return &Document{
		TimecodeScale: 1_000_000,
		Faults:        &media.Faults{},
		StoppedTracks: map[uint64]bool{},
	}
}

// TrackNumber reports whether the track is declared.
func (d *Document) TrackByNumber(n uint64) (*Track, bool) {
	for i := range d.Tracks {
		if d.Tracks[i].Number == n {
			return &d.Tracks[i], true
		}
	}
	return nil, false
}

// StopTrack marks a track halted and records why. Additional blocks for the
// track are skipped without aborting the remaining tracks.
func (d *Document) StopTrack(n uint64) { d.StoppedTracks[n] = true }

// Truncated reports whether the container was cut short.
func (d *Document) Truncated() bool { return d.truncated }

// MarkTruncated records a tail truncation; no final manifest may be written.
func (d *Document) MarkTruncated() {
	d.truncated = true
}

// TrackNumbers returns declared track numbers in ascending order.
func (d *Document) TrackNumbers() []uint64 {
	out := make([]uint64, len(d.Tracks))
	for i := range d.Tracks {
		out[i] = d.Tracks[i].Number
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Finalize converts source ticks into nanoseconds exactly and validates the
// declared duration. It is idempotent.
func (d *Document) Finalize() {
	if d.TimecodeScale <= 0 {
		d.Faults.Addf(media.LevelAbort, media.CodeBadTrack, 0, 0, "invalid timecode scale %d", d.TimecodeScale)
		d.TimecodeScale = 1_000_000
	}
	r := media.Rational{Num: d.TimecodeScale, Den: 1}
	for i := range d.Blocks {
		b := &d.Blocks[i]
		b.TimeNs = media.Nanos(b.Ticks, r)
		if b.DurationNs == 0 {
			continue
		}
	}
	for i := range d.Tracks {
		if d.Tracks[i].DefaultDurationNs < 0 {
			d.Tracks[i].DefaultDurationNs = 0
		}
	}
}

// Mode describes one root cause of an incomplete stream.
type Mode int

const (
	ModeOK Mode = iota
	ModeTruncatedVint
	ModeTruncatedElement
	ModeTooLarge
	ModeTooManyBlocks
	ModeDepth
)
