// Package model defines the core media units, the on-disk record layout and
// the locatable error model shared by every medpipe subsystem.
package model

// Record kinds stored inside a medpipe container.
const (
	KindVideo uint16 = 1
	KindAudio uint16 = 2
)

// Magic is the fixed container header prefix; Version is the only layout
// understood by this implementation.
const (
	Magic   = "MPIP"
	Version = byte(1)
)

// Frame is one decoded video record. Offset is the byte position of the
// record inside the source container and is used by locatable errors.
type Frame struct {
	Seq         uint32
	TimestampNS int64
	Width       int
	Height      int
	ColorMeta   string
	Keyframe    bool
	Metadata    []byte // canonical JSON as stored on disk
	Payload     []byte
	Checksum    [32]byte
	Offset      int64
}

// AudioBlock is one decoded PCM audio record.
type AudioBlock struct {
	Seq         uint32
	TimestampNS int64
	SampleRate  int
	Channels    int
	Samples     int
	Metadata    []byte
	Payload     []byte // little-endian signed 16-bit PCM
	Checksum    [32]byte
	Offset      int64
}

// VideoMeta is the JSON metadata attached to a video record.
type VideoMeta struct {
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Color     string `json:"color"`
	Keyframe  bool   `json:"keyframe"`
	FrameNum  uint32 `json:"frame_num"`
}

// AudioMeta is the JSON metadata attached to an audio record.
type AudioMeta struct {
	SampleRate int `json:"sample_rate"`
	Channels   int `json:"channels"`
	Samples    int `json:"samples"`
}

// Segment binds video frames and audio blocks that fall inside one shared
// clock window. References point at fingerprinted payload blobs.
type Segment struct {
	Index       int
	StartNS     int64
	EndNS       int64
	VideoSeqs   []uint32
	AudioSeqs   []uint32
	PayloadRefs []string
}

// SilenceInterval is a merged run of silent audio, positioned on the shared
// timeline. It never influences video cut points.
type SilenceInterval struct {
	StartNS int64
	EndNS   int64
	Samples int
}
