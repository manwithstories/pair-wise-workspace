package probe

import "mediacheck/internal/fault"

// Kind is the track media kind.
type Kind string

const (
	KindVideo Kind = "video"
	KindAudio Kind = "audio"
	KindOther Kind = "other"
)

// IndexCoverage records which sample-table boxes were usable.
type IndexCoverage struct {
	SampleSizes   bool `json:"sample_sizes"`
	SampleToChunk bool `json:"sample_to_chunk"`
	ChunkOffsets  bool `json:"chunk_offsets"`
	TimeToSample  bool `json:"time_to_sample"`
	SyncSamples   bool `json:"sync_samples"`
	Composition   bool `json:"composition_offsets"`
	Complete      bool `json:"complete"`
	// Rebuilt reports that chunk offsets were inferred from mdat bounds.
	Rebuilt bool `json:"rebuilt"`
}

// Track is the structural description of one track and its declared timeline.
type Track struct {
	ID         uint32        `json:"id"`
	Kind       Kind          `json:"kind"`
	Codec      string        `json:"codec"`
	Handler    string        `json:"handler"`
	Timescale  uint32        `json:"timescale"`
	DurationMS int64         `json:"duration_ms"`
	StartMS    int64         `json:"start_ms"`
	Index      IndexCoverage `json:"index"`
	Declared   uint64        `json:"declared_samples"`
	Walked     uint64        `json:"walked_samples"`
	Keyframes  int           `json:"keyframes"`

	MaxKeyframeGapMS  int64 `json:"max_keyframe_gap_ms"`
	MeanKeyframeGapMS int64 `json:"mean_keyframe_gap_ms"`
	MinKeyframeGapMS  int64 `json:"min_keyframe_gap_ms"`

	FirstSample      int64  `json:"first_sample_offset"`
	DataEnd          int64  `json:"data_end_offset"`
	UnreadableOffset int64  `json:"unreadable_offset"`
	TruncatedSamples uint64 `json:"truncated_samples"`

	Issues []*fault.Fault `json:"-"`

	table *sampleTable
	cfg   Config
	mdats []Bounds
	size  int64
	// keyTimes holds up to MaxKeyframes keyframe DTS values in milliseconds.
	keyTimes       []int64
	maxGap         int64
	movieTimescale uint32
	minGap         int64
	gapSum         int64
	gapCount       int64
}

// File is the probe result for one input.
type File struct {
	Path            string         `json:"path"`
	Size            int64          `json:"size"`
	Boxes           []Box          `json:"boxes"`
	MOOFCount       int            `json:"moof_count"`
	Tracks          []*Track       `json:"tracks"`
	Truncated       bool           `json:"truncated"`
	FirstUnreadable int64          `json:"first_unreadable_offset"`
	DiscardedBytes  int64          `json:"discarded_bytes"`
	Fragmented      bool           `json:"fragmented"`
	Issues          []*fault.Fault `json:"-"`
}

// TrackIssues flattens every fault from the file and its tracks.
func (f *File) TrackIssues() []*fault.Fault {
	out := append([]*fault.Fault{}, f.Issues...)
	for _, t := range f.Tracks {
		out = append(out, t.Issues...)
	}
	return out
}
