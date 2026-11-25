package media

import "crypto/sha256"

// Codec identifies an accepted elementary codec.
type Codec string

const (
	CodecH264 Codec = "h264"
	CodecAAC  Codec = "aac-lc"
)

// TrackKind separates audio and video.
type TrackKind string

const (
	TrackVideo TrackKind = "video"
	TrackAudio TrackKind = "audio"
)

// Track is a validated elementary track and its TS PID.
type Track struct {
	ID        int       `json:"id"`
	PID       uint16    `json:"pid"`
	Kind      TrackKind `json:"kind"`
	Codec     Codec     `json:"codec"`
	Timescale uint32    `json:"timescale"`
}

// Sample is one compressed access unit. Its Data is never decoded.
type Sample struct {
	TrackID      int    `json:"track_id"`
	PID          uint16 `json:"pid"`
	Index        int    `json:"index"`
	DTS          int64  `json:"dts"`
	PTS          int64  `json:"pts"`
	Duration     uint32 `json:"duration"`
	Data         []byte `json:"-"`
	RandomAccess bool   `json:"random_access"`
}

// Hash returns the stable per-sample payload hash used across remux outputs.
func (s Sample) Hash() string {
	sum := sha256.Sum256(s.Data)
	return formatHex(sum[:])
}

func formatHex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0x0f]
	}
	return string(out)
}

// SampleRef is the hash/timing identity stored in manifests.
type SampleRef struct {
	TrackID  int    `json:"track_id"`
	Index    int    `json:"index"`
	DTS      int64  `json:"dts"`
	PTS      int64  `json:"pts"`
	Duration uint32 `json:"duration"`
	Size     int    `json:"size"`
	SHA256   string `json:"sha256"`
}

// Ref creates a manifest reference without retaining payload bytes.
func (s Sample) Ref() SampleRef {
	return SampleRef{
		TrackID:  s.TrackID,
		Index:    s.Index,
		DTS:      s.DTS,
		PTS:      s.PTS,
		Duration: s.Duration,
		Size:     len(s.Data),
		SHA256:   s.Hash(),
	}
}

// SupportedCodec reports whether codec pass-through is implemented.
func SupportedCodec(c Codec) bool {
	return c == CodecH264 || c == CodecAAC
}
