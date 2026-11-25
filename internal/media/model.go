package media

import (
	"crypto/sha256"
	"fmt"
	"sort"
)

// Kind distinguishes container track kinds.
type Kind string

const (
	KindVideo Kind = "video"
	KindAudio Kind = "audio"
	KindOther Kind = "other"
)

// TrackKey is the stable identity used to map camera feeds. Two container
// tracks sharing a key are a conflict; a required key with no track is a
// missing-track failure, and either rejects output submission.
type TrackKey struct {
	Camera   string `json:"camera"`
	Language string `json:"language"`
	Codec    string `json:"codec"`
	Role     string `json:"role"`
}

func (k TrackKey) String() string {
	return fmt.Sprintf("%s/%s/%s/%s", k.Camera, k.Language, k.Codec, k.Role)
}

// Less orders keys deterministically for reproducible output.
func (k TrackKey) Less(o TrackKey) bool {
	if k.Camera != o.Camera {
		return k.Camera < o.Camera
	}
	if k.Role != o.Role {
		return k.Role < o.Role
	}
	if k.Language != o.Language {
		return k.Language < o.Language
	}
	return k.Codec < o.Codec
}

// Track is a container track after mapping to a unified key.
type Track struct {
	Key         TrackKey `json:"key"`
	ID          uint32   `json:"id"`
	Kind        Kind     `json:"kind"`
	Timescale   int64    `json:"timescale"`
	EntryType   string   `json:"entry_type"` // e.g. "avc1", "hvc1", "mp4a"
	SampleEntry []byte   `json:"-"`          // raw stsd child, preserved verbatim
	InitSegment string   `json:"init_segment,omitempty"`
}

// SourceRef records where a sample came from, for the output source index.
type SourceRef struct {
	Track   string    `json:"track"`
	Segment int       `json:"segment"`
	Sample  int       `json:"sample"`
	URI     string    `json:"uri,omitempty"`
	Pos     SourcePos `json:"pos,omitempty"`
}

// Sample is one coded access unit on the unified 90 kHz axis. DTS, PTS and
// Duration are unified ticks; Data holds the encoded bytes untouched.
type Sample struct {
	Key      TrackKey  `json:"key"`
	DTS      int64     `json:"dts"`
	PTS      int64     `json:"pts"`
	Duration int64     `json:"duration"`
	Sync     bool      `json:"sync"`
	Data     []byte    `json:"-"`
	Source   SourceRef `json:"source"`
	Hash     [32]byte  `json:"-"`
}

// HashBytes returns the SHA-256 of the sample payload, computed once.
func (s *Sample) HashBytes() [32]byte {
	if s.Hash == ([32]byte{}) && len(s.Data) > 0 {
		s.Hash = sha256.Sum256(s.Data)
	}
	return s.Hash
}

// ClonePayload deep-copies sample data so readers can release segment buffers.
func (s Sample) ClonePayload() Sample {
	out := s
	out.Data = append([]byte(nil), s.Data...)
	return out
}

// Limits bounds every external input so hostile or corrupt fixtures cannot
// exhaust memory. Zero fields fall back to DefaultLimits.
type Limits struct {
	MaxManifestBytes int64 `json:"max_manifest_bytes"`
	MaxSegments      int   `json:"max_segments"`
	MaxPackets       int   `json:"max_packets"`
	MaxPacketBytes   int   `json:"max_packet_bytes"`
	MaxSegmentBytes  int64 `json:"max_segment_bytes"`
	MaxSamplesPerSeg int   `json:"max_samples_per_segment"`
	MaxTrackCount    int   `json:"max_track_count"`
}

// DefaultLimits is used by the CLI unless overridden.
func DefaultLimits() Limits {
	return Limits{
		MaxManifestBytes: 8 << 20,
		MaxSegments:      10000,
		MaxPackets:       1 << 20,
		MaxPacketBytes:   64 << 10,
		MaxSegmentBytes:  64 << 20,
		MaxSamplesPerSeg: 10000,
		MaxTrackCount:    64,
	}
}

// Check verifies a value against a limit; negative limits are ignored.
func (l Limits) CheckBytes(name string, n int64) error {
	var max int64
	switch name {
	case "manifest":
		max = l.MaxManifestBytes
	case "segment":
		max = l.MaxSegmentBytes
	case "packet":
		max = int64(l.MaxPacketBytes)
	}
	if max > 0 && n > max {
		return New(CodeLimit, name, SourcePos{}, "size %d exceeds limit %d", n, max)
	}
	return nil
}

// CheckCount verifies a count against a limit.
func (l Limits) CheckCount(name string, n int) error {
	var max int
	switch name {
	case "segments":
		max = l.MaxSegments
	case "packets":
		max = l.MaxPackets
	case "samples":
		max = l.MaxSamplesPerSeg
	case "tracks":
		max = l.MaxTrackCount
	}
	if max > 0 && n > max {
		return New(CodeLimit, name, SourcePos{}, "count %d exceeds limit %d", n, max)
	}
	return nil
}

// SortSamples sorts by decode time, then presentation time, for determinism.
func SortSamples(s []Sample) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].DTS != s[j].DTS {
			return s[i].DTS < s[j].DTS
		}
		return s[i].PTS < s[j].PTS
	})
}
