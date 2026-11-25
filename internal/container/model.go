// Package container demuxes the constrained MP4/fMP4 archive profile and
// remuxes samples without decoding or transcoding their payload bytes.
package container

import (
	"crypto/sha256"
	"encoding/hex"

	"archivectl/internal/timeline"
)

// Track kinds use stable English codes in manifests; Chinese labels are a
// presentation concern of the CLI/report.
const (
	KindVideo    = "video"
	KindAudio    = "audio"
	KindSubtitle = "subtitle"
	KindChapters = "chapters"
	KindMetadata = "metadata"
)

// Event kinds correspond to the timed markers the archivists must recover.
const (
	EventSubtitle = "subtitle"
	EventChapter  = "chapter"
	EventID3      = "id3"
	EventEMSG     = "emsg"
	EventSEI      = "sei"
)

// Sample is one preserved media sample. DTS and PTS remain in the original
// track timescale; TimeNS is the normalized cross-track manifest value.
type Sample struct {
	Index      int    `json:"index"`
	Data       []byte `json:"-"`
	Size       int    `json:"size"`
	Hash       string `json:"sha256"`
	DTS        int64  `json:"dts"`
	PTS        int64  `json:"pts"`
	Duration   int64  `json:"duration"`
	Flags      uint32 `json:"flags"`
	FileOffset int64  `json:"file_offset"`
	TimeNS     int64  `json:"time_ns"`
}

// Track contains samples and legal references to other tracks.
type Track struct {
	ID         uint32   `json:"id"`
	Kind       string   `json:"kind"`
	Codec      string   `json:"codec"`
	Handler    string   `json:"handler"`
	Timescale  uint32   `json:"timescale"`
	Origin     int64    `json:"origin"`
	Duration   int64    `json:"duration"`
	References []uint32 `json:"references,omitempty"`
	Samples    []Sample `json:"samples"`
}

// Event is a timed metadata marker with attribution to a track/sample.
type Event struct {
	Kind        string `json:"kind"`
	TrackID     uint32 `json:"track_id"`
	SampleIndex int    `json:"sample_index"`
	RawTime     int64  `json:"raw_time"`
	Timescale   uint32 `json:"timescale"`
	TimeNS      int64  `json:"time_ns"`
	Label       string `json:"label,omitempty"`
	Payload     []byte `json:"-"`
	RawBox      []byte `json:"-"`
	Hash        string `json:"sha256"`
	ByteOffset  int64  `json:"byte_offset"`
}

// BoxReport locates a recognized MP4 box.
type BoxReport struct {
	Type       string `json:"type"`
	Offset     int64  `json:"offset"`
	Size       int64  `json:"size"`
	HeaderSize int    `json:"header_size"`
}

// Fragment is one parsed movie fragment.
type Fragment struct {
	Sequence uint32
	TrackID  uint32
	BaseDTS  int64
	Offset   int64
	Samples  []Sample
}

// Asset is the demuxed archive document.
type Asset struct {
	Path        string      `json:"path"`
	Fragmented  bool        `json:"fragmented"`
	Tracks      []Track     `json:"tracks"`
	Events      []Event     `json:"events"`
	Boxes       []BoxReport `json:"boxes"`
	Fragments   []Fragment  `json:"fragments,omitempty"`
	Partial     bool        `json:"partial"`
	GapRanges   int         `json:"gap_ranges"`
	GapPackets  int         `json:"gap_packets"`
	SourceBytes int64       `json:"source_bytes"`
}

// HashBytes returns the stable SHA-256 used for payload and output checks.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func ensureSampleHash(s *Sample) {
	if s.Size == 0 {
		s.Size = len(s.Data)
	}
	if s.Hash == "" && len(s.Data) > 0 {
		s.Hash = HashBytes(s.Data)
	}
}

// TrackByID returns a pointer to a track and whether it exists.
func (a *Asset) TrackByID(id uint32) (*Track, bool) {
	for i := range a.Tracks {
		if a.Tracks[i].ID == id {
			return &a.Tracks[i], true
		}
	}
	return nil, false
}

// ValidateReferences rejects track references that do not name a track.
func (a *Asset) ValidateReferences() error {
	ids := make(map[uint32]struct{}, len(a.Tracks))
	for _, t := range a.Tracks {
		ids[t.ID] = struct{}{}
	}
	for _, t := range a.Tracks {
		for _, ref := range t.References {
			if _, ok := ids[ref]; !ok {
				return &timeline.Error{
					Code:    timeline.CodeIllegalReference,
					Op:      "validate track reference",
					TrackID: t.ID,
					Offset:  -1,
					Detail:  "reference to missing track id",
				}
			}
		}
	}
	return nil
}
