// Package media defines the unified frame/audio data model, the 90 kHz media
// clock and the locatable error model shared by every subsystem.
package media

// Frame is the unified representation of one parsed image-frame record.
type Frame struct {
	// Sequence is the zero-based frame sequence number.
	Sequence uint32
	// PTS90k is the presentation timestamp on the 90 kHz clock.
	PTS90k uint64
	// PTSConfidence marks PTS values derived by fallback rather than
	// read from a trusted index.
	PTSConfidence Confidence
	// Payload is the image-frame payload (already checksum-verified).
	Payload []byte
	// Corrupt marks a frame whose record could not be trusted (checksum
	// mismatch, PTS regression). Such entries are timeline gaps, never
	// silently rewritten.
	Corrupt bool
	// GapReason describes why the frame is marked corrupt, if any.
	GapReason string
}

// AudioBlock is one PCM block aligned 1:1 with a frame sequence number.
type AudioBlock struct {
	// Sequence matches the frame sequence this block is aligned to.
	Sequence uint32
	// PTS90k is the presentation timestamp on the shared 90 kHz clock.
	PTS90k uint64
	// SampleRate is the PCM sample rate in Hz.
	SampleRate uint32
	// Layout identifies the channel layout of Samples.
	Layout ChannelLayout
	// Samples holds interleaved PCM samples in the range [-32768, 32767].
	Samples []int16
}

// Confidence describes how a timestamp was obtained.
type Confidence uint8

const (
	// ConfidenceVerified: timestamp came from a checksum-verified index/record.
	ConfidenceVerified Confidence = iota
	// ConfidenceDerived: timestamp was derived from continuous PTS stepping.
	ConfidenceDerived
)

// String renders the confidence for terminal/metric output.
func (c Confidence) String() string {
	switch c {
	case ConfidenceVerified:
		return "verified"
	case ConfidenceDerived:
		return "derived"
	default:
		return "unknown"
	}
}

// ChannelLayout identifies a PCM channel layout.
type ChannelLayout uint8

const (
	LayoutMono ChannelLayout = iota + 1
	LayoutStereo
	LayoutFiveOne
)

// Channels returns the number of interleaved channels for the layout.
func (l ChannelLayout) Channels() int {
	switch l {
	case LayoutMono:
		return 1
	case LayoutStereo:
		return 2
	case LayoutFiveOne:
		return 6
	default:
		return 0
	}
}

// String renders the layout for terminal/metric output.
func (l ChannelLayout) String() string {
	switch l {
	case LayoutMono:
		return "mono"
	case LayoutStereo:
		return "stereo"
	case LayoutFiveOne:
		return "5.1"
	default:
		return "unknown"
	}
}
