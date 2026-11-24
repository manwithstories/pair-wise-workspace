package model

import "fmt"

// TrackKind distinguishes the media families the kernel maps by rule.
type TrackKind uint8

const (
	KindUnknownTrack TrackKind = iota
	KindVideo
	KindAudio
)

func (k TrackKind) String() string {
	switch k {
	case KindVideo:
		return "video"
	case KindAudio:
		return "audio"
	default:
		return "unknown"
	}
}

// ParseTrackKind parses manifest spellings ("video"/"audio").
func ParseTrackKind(s string) (TrackKind, error) {
	switch s {
	case "video":
		return KindVideo, nil
	case "audio":
		return KindAudio, nil
	default:
		return KindUnknownTrack, New(KindInvalidInput, "model", "parse track kind",
			"unknown track kind "+s)
	}
}

// Packet is the unified demux record for MP4 samples and RTP packets.
// Timestamps stay in the source track timescale and keep their original
// (possibly non-zero) origin; different time bases are never added here.
type Packet struct {
	SourceID  string    // manifest material id
	TrackID   uint32    // source track id inside the material
	Kind      TrackKind // media family
	Codec     string    // e.g. "avc1", "mp4a"
	Seq       int64     // 0-based source packet index (error position)
	DTS       int64     // decode timestamp in source timescale ticks
	PTS       int64     // presentation timestamp in source timescale ticks
	Duration  int64     // packet duration in source timescale ticks
	Timescale int64     // source track timescale (ticks per second)
	Keyframe  bool      // true only for real sync samples
	Payload   []byte    // copied media bytes; never re-encoded
}

// TrackInfo describes one demuxed source track.
type TrackInfo struct {
	ID        uint32
	Kind      TrackKind
	Codec     string
	Timescale int64
	Packets   int64
	// FirstDTS preserves a non-zero track origin.
	FirstDTS int64
	LastDTS  int64
}

// SourceRange records exactly which input packets back an output segment.
type SourceRange struct {
	MaterialID string
	TrackID    uint32
	FirstSeq   int64
	LastSeq    int64 // inclusive
	StartDTS   int64
	EndDTS     int64 // inclusive, source timescale ticks
	Timescale  int64
	Packets    int64
}

// TrackMapping is one output-slot -> source-track assignment.
type TrackMapping struct {
	OutputSlot  int
	Kind        TrackKind
	MaterialID  string
	SourceTrack uint32
	Codec       string
}

func (m TrackMapping) key() string {
	return fmt.Sprintf("%s:%d", m.MaterialID, m.SourceTrack)
}

// SegmentRecord is the per-output provenance entry consumed by checkpoint
// and verification: input ranges, mapping, packet count and digest.
type SegmentRecord struct {
	Index       int
	OutputFile  string
	Ranges      []SourceRange
	Mappings    []TrackMapping
	PacketCount int64
	SHA256Hex   string
	Bytes       int64
}

// InputDigest identifies a manifest run for checkpoint validation. When the
// digest changes, resuming is refused (KindConflict).
type InputDigest struct {
	ManifestSHA256 string
	SourceSHA256   map[string]string // material id -> file digest
}
