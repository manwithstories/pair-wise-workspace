// Package inspect identifies H.264 and ADTS AAC frame boundaries inside RLY1
// record payloads and measures keyframe intervals on the extended 90 kHz
// timeline.
package inspect

// Codec names reported in the stream summary.
const (
	CodecH264 = "h264"
	CodecAAC  = "aac"
	CodecRaw  = "raw"
)

// H.264 NAL unit types that matter to the auditor.
const (
	nalIDR = 5
	nalSPS = 7
	nalPPS = 8
)

// FrameType classifies an access unit.
type FrameType int

const (
	// FrameUnknown means the payload did not carry a recognisable codec frame.
	FrameUnknown FrameType = iota
	// FrameIDR is an H.264 instantaneous decoder refresh access unit.
	FrameIDR
	// FrameNonIDR is any other H.264 access unit.
	FrameNonIDR
	// FrameAAC is one ADTS audio frame.
	FrameAAC
)

func (f FrameType) String() string {
	switch f {
	case FrameIDR:
		return "IDR"
	case FrameNonIDR:
		return "P/B"
	case FrameAAC:
		return "AAC"
	default:
		return "raw"
	}
}

// Keyframe reports whether the frame is a random access point.
func (f FrameType) Keyframe() bool { return f == FrameIDR }

// Frame is one identified media frame.
type Frame struct {
	Track   uint8
	Type    FrameType
	Offset  int64 // payload byte offset within the segment
	Ticks   uint64
	Size    int
	SPSPPS  bool
	NalType uint8
	ADTSHz  int
}
