package parser

import "mediascan/internal/media"

// ByteRange is a validated payload extent, currently one mdat box.
type ByteRange struct {
	Offset int64 `json:"offset"`
	End    int64 `json:"end"`
}

// OffsetEntry retains both the stco/co64 value and the exact file offset of
// that value, enabling zero-error corruption coordinates.
type OffsetEntry struct {
	Value       uint64 `json:"value"`
	ValueOffset int64  `json:"value_offset"`
	Wide        bool   `json:"wide"`
}

// Edit is a compact edts/elst entry.
type Edit struct {
	SegmentDuration uint64 `json:"segment_duration"`
	MediaTime       int64  `json:"media_time"`
	MediaRate       uint32 `json:"media_rate"`
	ByteOffset      int64  `json:"byte_offset"`
}

// Sample is an expanded ordinary sample-table entry.
type Sample struct {
	Number     int    `json:"number"`
	DecodeTime int64  `json:"decode_time"`
	Delta      uint32 `json:"delta"`
	Size       uint64 `json:"size"`
	Offset     uint64 `json:"offset"`
	Keyframe   bool   `json:"keyframe"`
}

// FragmentSample is one trun entry.
type FragmentSample struct {
	Number   int    `json:"number"`
	Duration uint32 `json:"duration"`
	Size     uint32 `json:"size"`
	Flags    uint32 `json:"flags"`
	Keyframe bool   `json:"keyframe"`
}

// Fragment is one moof/traf/tfdt/trun sequence.
type Fragment struct {
	MoofOffset int64            `json:"moof_offset"`
	TfdtOffset int64            `json:"tfdt_offset"`
	TrackID    uint32           `json:"track_id"`
	BaseTime   int64            `json:"base_time"`
	Version    byte             `json:"version"`
	DataOffset int32            `json:"data_offset"`
	TrunOffset int64            `json:"trun_offset"`
	Samples    []FragmentSample `json:"samples"`
}

// Track is the parser-level sample table/fragment model.
type Track struct {
	TrackID          uint32        `json:"track_id"`
	Timescale        uint32        `json:"timescale"`
	Duration         uint64        `json:"duration"`
	Handler          string        `json:"handler"`
	MDHDOffset       int64         `json:"mdhd_offset"`
	Edits            []Edit        `json:"edits,omitempty"`
	Samples          []Sample      `json:"samples,omitempty"`
	KeyframeNumbers  []uint32      `json:"keyframe_numbers,omitempty"`
	ChunkOffsets     []OffsetEntry `json:"chunk_offsets,omitempty"`
	Fragments        []Fragment    `json:"fragments,omitempty"`
	SampleCountKnown bool          `json:"sample_count_known"`

	sampleCount uint32
	uniformSize uint32
	timeEntries []TimeEntry
	stscEntries []STSCEntry
}

// File is a parsed asset: box index, tracks, mdat extents, and all faults.
type File struct {
	Source *Source
	Index  *BoxIndex
	Tracks []*Track
	MDAT   []ByteRange
	Faults []media.Fault
}
