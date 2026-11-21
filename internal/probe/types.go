// Package probe parses ISO BMFF assets into unified-timeline track events
// and integrity findings. It never reads past a declared box boundary and
// distinguishes container truncation, corrupt samples and index defects.
package probe

import (
	"io"

	"github.com/abema/go-mp4"
)

// Sample is one indexed sample expressed on both its native timescale and
// the shared 90 kHz timeline (see internal/clock).
type Sample struct {
	Index       uint32 // 0-based within the track
	IsKeyframe  bool
	Size        uint32
	ChunkOffset int64
	DTS         int64 // decode time in track timescale units
	PTS         int64 // presentation time after edit list, in track units
	StartTicks  int64 // PTS mapped onto the shared timeline
	EndTicks    int64 // StartTicks + duration mapped onto the shared timeline
}

// Edit is a normalized edit-list entry in track-timescale units. Empty
// edits (MediaTime < 0) carry no media and only shift the timeline origin.
type Edit struct {
	SegmentDuration int64
	MediaTime       int64 // -1 means empty edit
	MediaRate       int64
}

// Track is the unified-timeline view of one trak box.
type Track struct {
	ID         uint32
	IsVideo    bool
	Timescale  uint32
	Samples    []Sample
	Edits      []Edit
	StartDTS   int64 // first sample decode time in track units
	Duration   int64 // media duration in track units
	TimelineStart int64 // first presentation tick on the shared timeline
}

// BoxSpan records a top-level box and its declared byte span so callers can
// verify container integrity without trusting declared sizes blindly.
type BoxSpan struct {
	Type   string
	Offset int64
	Size   int64
}

// IntegrityReport collects structural findings and verified box spans.
type IntegrityReport struct {
	FileSize        int64
	Boxes           []BoxSpan
	Mdat            *BoxSpan
	Defects         []probeDefect
	IndexIncomplete bool // stco/stsz page missing or counts inconsistent
}

type probeDefect struct {
	code   string
	start  int64 // shared-tick range when known, else byte offset encoded negative-safe
	end    int64
	detail string
}

// rawIndex holds the tables read from stbl while walking the box tree.
type rawIndex struct {
	trackID    uint32
	isVideo    bool
	timescale  uint32
	duration   int64
	stts       []mp4.SttsEntry
	keyframes  map[uint32]struct{}
	sizes      []uint32
	chunks     []int64
	samplesPerChunk []uint32 // firstChunk, samplesPerChunk pairs
	edits      []Edit
	haveSTSS   bool
}

// limitedReaderAt guards every sample read: the parser must never cross a
// declared box boundary, even when an index forges an oversized length.
type limitedReaderAt struct {
	r         io.ReaderAt
	hardLimit int64
}

func (l *limitedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= l.hardLimit {
		return 0, io.EOF
	}
	if int64(len(p)) > l.hardLimit-off {
		p = p[:l.hardLimit-off]
	}
	return l.r.ReadAt(p, off)
}

// boundedSampleReader returns a ReaderAt clamped to one mdat box, the
// hard boundary no sample read is allowed to cross.
func boundedSampleReader(r io.ReaderAt, mdat *BoxSpan) io.ReaderAt {
	if mdat == nil {
		return &limitedReaderAt{r: r, hardLimit: 0}
	}
	return &limitedReaderAt{r: r, hardLimit: mdat.Offset + mdat.Size}
}
