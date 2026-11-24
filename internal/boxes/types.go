// Package boxes scans CMAF/fMP4 initialization and media segments.  It never
// reads whole mdat payloads into memory; metadata boxes are bounded and media
// payloads are streamed while hashing.
package boxes

import "bcast/internal/media"

// MaxMetadataBox limits allocation for a non-media box such as moof/trun.
const MaxMetadataBox = 64 << 20

// InitInfo contains the track IDs and timescales found in an init segment.
type InitInfo struct {
	Path       string            `json:"path"`
	Timescales map[uint32]uint32 `json:"timescales"`
}

// Fragment is one scanned media segment.  A segment file normally carries one
// track in this project's fixtures; separate files are used for A/V tracks.
type Fragment struct {
	No             int                  `json:"no"`
	Path           string               `json:"path"`
	Sequence       uint32               `json:"sequence"`
	TrackID        uint32               `json:"track_id"`
	Timescale      uint32               `json:"timescale"`
	MoofOffset     int64                `json:"moof_offset"`
	MoofSize       int64                `json:"moof_size"`
	TFDT           uint64               `json:"tfdt"`
	TFDTValueOff   int64                `json:"tfdt_value_offset"`
	TFDTValueWidth int                  `json:"tfdt_value_width"`
	DataOffset     int64                `json:"data_offset"`
	DataStart      int64                `json:"data_start"`
	DataEnd        int64                `json:"data_end"`
	PayloadBytes   int64                `json:"payload_bytes"`
	Samples        []media.SampleTiming `json:"samples"`
	ContentSHA256  string               `json:"content_sha256"`
	Isolated       bool                 `json:"isolated"`
	Error          *media.Error         `json:"error,omitempty"`
}

// Loc is a fragment/byte location used in sequence anomalies.
type Loc struct {
	TrackID  uint32 `json:"track_id"`
	Sequence uint32 `json:"sequence"`
	Fragment int    `json:"fragment"`
	Offset   int64  `json:"offset"`
	Path     string `json:"path"`
}

// MissingSequences lists missing mfhd numbers for one track.
type MissingSequences struct {
	TrackID   uint32   `json:"track_id"`
	Sequences []uint32 `json:"sequences"`
}

// SequenceReport summarizes mfhd ordering and duplication.
type SequenceReport struct {
	Missing          []MissingSequences `json:"missing"`
	OutOfOrder       []Loc              `json:"out_of_order"`
	DuplicateMFHD    []Loc              `json:"duplicate_mfhd"`
	ContentDuplicate []Loc              `json:"content_duplicate"`
}

// MissingCount returns the total number of missing sequence numbers.
func (s SequenceReport) MissingCount() int {
	n := 0
	for _, m := range s.Missing {
		n += len(m.Sequences)
	}
	return n
}

// DuplicateCount counts content duplicates (the copies excluded from index).
func (s SequenceReport) DuplicateCount() int { return len(s.ContentDuplicate) }

// BatchReport is the deterministic result of scanning one batch directory.
type BatchReport struct {
	Dir       string         `json:"dir"`
	Init      InitInfo       `json:"init"`
	Files     []string       `json:"files"`
	Fragments []*Fragment    `json:"fragments"`
	Unique    []*Fragment    `json:"-"`
	Isolated  []*Fragment    `json:"isolated"`
	Sequences SequenceReport `json:"sequences"`
	Fatal     *media.Error   `json:"fatal,omitempty"`
}
