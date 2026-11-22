// Package stream analyzes MP4 track samples and RTP packet sequences,
// translating structural faults into the unified media error model so a
// damaged region can be isolated without failing the whole track.
package stream

// Limits bound analysis work per input and per track.
type Limits struct {
	MaxInputBytes int64
	MaxTrackBytes int64
	MaxSamples    int
	MaxPackets    int
}

// DefaultLimits keep generated fixtures and accidental large inputs bounded.
var DefaultLimits = Limits{
	MaxInputBytes: 64 << 20,
	MaxTrackBytes: 64 << 20,
	MaxSamples:    200_000,
	MaxPackets:    100_000,
}

func (l Limits) normalize() Limits {
	if l.MaxInputBytes <= 0 {
		l.MaxInputBytes = DefaultLimits.MaxInputBytes
	}
	if l.MaxTrackBytes <= 0 {
		l.MaxTrackBytes = DefaultLimits.MaxTrackBytes
	}
	if l.MaxSamples <= 0 {
		l.MaxSamples = DefaultLimits.MaxSamples
	}
	if l.MaxPackets <= 0 {
		l.MaxPackets = DefaultLimits.MaxPackets
	}
	return l
}

// Sample is one decoded MP4 sample's time and size facts.
type Sample struct {
	// Number is the 1-based sample number as used by stss/stsz.
	Number int
	// Offset is the absolute byte offset of the sample payload in the file.
	Offset int64
	// Size is the declared sample size in bytes.
	Size int64
	// DTSNs/EndNs are decode-time bounds on the shared nanosecond base.
	DTSNs int64
	EndNs int64
	// DurationNs is EndNs - DTSNs.
	DurationNs int64
	// Sync reports whether the sample is a keyframe (sync sample).
	Sync bool
}

// Track is the analysis result for a single MP4 track.
type Track struct {
	TrackID     uint32
	Kind        string // "video" or "audio"
	Timescale   uint32
	DurationNs  int64
	Samples     []Sample
	Keyframes   []int // 1-based sample numbers of sync samples
	IndexSource string
	Complete    bool
}
