package fixture

import (
	"relayaudit/internal/clock"
	"relayaudit/internal/wire"
)

const (
	videoFrameTicks = 3600 // 25 fps at 90 kHz
	audioFrameTicks = 1920 // 1024 samples at 48 kHz
	idrEveryFrames  = 50   // one IDR every 2 seconds
)

// Tracks of the four-track reference segment.
const (
	TrackProgramVideo uint8 = 1
	TrackBackupVideo  uint8 = 2
	TrackProgramAudio uint8 = 3
	TrackBackupAudio  uint8 = 4
)

// Spec selects the generated data and the damage applied to it.
type Spec struct {
	Seed int64
	Size int

	// TruncateTail removes this many bytes from the end of the segment.
	TruncateTail int
	// CorruptRecord flips one payload byte in this 1-based record.
	CorruptRecord int
	// DropIndexPage blanks this 1-based index page.
	DropIndexPage int
}

// Damage records exactly where damage was injected, in original file
// coordinates so that detected offsets can be compared byte for byte.
type Damage struct {
	TruncateTail      int
	CleanBytes        int
	CorruptRecord     int
	CorruptRecordOff  int64
	CorruptByteOff    int64
	DropIndexPage     int
	DroppedPageOffset int64
}

// Segment is a generated file plus its undamaged record inventory.
type Segment struct {
	Bytes []byte
	// Records is the undamaged record list in file order, with offsets.
	Records []*wire.Record
	// Entries is the undamaged index.
	Entries []wire.Entry
	// DurationTicks is the media duration covered by the generated records.
	DurationTicks uint64
	// BaseRaw is the raw 32-bit PTS of the first frame on every track.
	BaseRaw clock.Stamp
	// TrackCount is the number of multiplexed tracks.
	TrackCount int
	Damage     Damage
}

// SourceSize returns the byte length of the generated file.
func (s *Segment) SourceSize() int64 { return int64(len(s.Bytes)) }

// mediaCursor tracks the next PTS and frame counters per media stream.
type mediaCursor struct {
	next [4]uint64
	n    [4]uint64
}

// candidates returns the pending PTS per track index.
func (m *mediaCursor) candidates() [4]uint64 { return m.next }

func (m *mediaCursor) advance(i int) {
	if i < 2 {
		m.next[i] += videoFrameTicks
	} else {
		m.next[i] += audioFrameTicks
	}
	m.n[i]++
}

// paced is one planned record before its payload is built.
type paced struct {
	track uint8
	pts   uint64
	size  int
}
