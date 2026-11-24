package mp4

// AudioTrack is normalized stereo PCM ready for MP4 muxing. Samples are
// grouped in packet-sized chunks for stable sample indexing.
type AudioTrack struct {
	SampleRate int
	Channels   int
	Bits       int
	Samples    [][]byte // each sample is interleaved little-endian PCM
	Durations  []uint32 // in SampleRate timescale, usually one common delta
	Timescale  uint32
}

// VideoSample is one length-prefixed AVCC frame payload source. NAL units are
// supplied without start codes; Mux converts them to 4-byte lengths.
type VideoSample struct {
	NALs  [][]byte
	Delta uint32
	Sync  bool
}

// VideoTrack contains supported constrained H.264 samples and avcC bytes.
type VideoTrack struct {
	Width     uint16
	Height    uint16
	Timescale uint32
	SPS       []byte
	PPS       []byte
	Samples   []VideoSample
}

// MuxConfig is the complete normalized MP4 input.
type MuxConfig struct {
	Audio AudioTrack
	Video VideoTrack
	Color ColorDecision
}

// BoxInfo is a boundary record verified with go-mp4.
type BoxInfo struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Offset uint64 `json:"offset"`
	Size   uint64 `json:"size"`
}

// Validation contains MP4 structural and sample-index checks.
type Validation struct {
	Boxes           []BoxInfo `json:"boxes"`
	AudioSamples    int       `json:"audio_samples"`
	VideoSamples    int       `json:"video_samples"`
	AudioDuration   uint64    `json:"audio_duration"`
	VideoDuration   uint64    `json:"video_duration"`
	MDATSize        uint64    `json:"mdat_size"`
	BoundariesValid bool      `json:"boundaries_valid"`
}
