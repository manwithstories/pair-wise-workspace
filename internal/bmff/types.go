package bmff

import (
	"fmt"

	"mediad/internal/model"
)

const IndexVersion = 1

// BoxHeader is an exact located BMFF/MOV box. Offset is the first size byte.
type BoxHeader struct {
	Type          string `json:"type"`
	Depth         int    `json:"depth"`
	Offset        int64  `json:"offset"`
	HeaderSize    int64  `json:"header_size"`
	Size          int64  `json:"size"`
	PayloadOffset int64  `json:"payload_offset"`
	End           int64  `json:"end"`
}

func (b BoxHeader) Contains(off int64) bool {
	return off >= b.PayloadOffset && off < b.End
}

// Edit is one edit-list entry in movie timescale units.
type Edit struct {
	SegmentDuration int64 `json:"segment_duration"`
	MediaTime       int64 `json:"media_time"`
	MediaRateInt    int16 `json:"media_rate_int"`
	MediaRateFrac   int16 `json:"media_rate_frac"`
}

// Sample is one exact located media sample. Timestamps are integer ticks in
// Track.Timescale; PTS includes stts and ctts and is never accumulated as a
// floating-point value.
type Sample struct {
	Index    int    `json:"index"`
	TrackID  uint32 `json:"track_id"`
	Offset   int64  `json:"offset"`
	Size     uint32 `json:"size"`
	DTS      int64  `json:"dts"`
	PTS      int64  `json:"pts"`
	Duration uint32 `json:"duration"`
	Sync     bool   `json:"sync"`
}

type Track struct {
	ID               uint32   `json:"id"`
	Handler          string   `json:"handler"`
	Codec            string   `json:"codec"`
	Timescale        uint32   `json:"timescale"`
	Duration         uint64   `json:"duration"`
	Width            uint16   `json:"width,omitempty"`
	Height           uint16   `json:"height,omitempty"`
	SampleEntry      []byte   `json:"sample_entry,omitempty"`
	Encrypted        bool     `json:"encrypted"`
	EncryptionScheme string   `json:"encryption_scheme,omitempty"`
	Edits            []Edit   `json:"edits,omitempty"`
	Samples          []Sample `json:"samples"`
}

// Index is the deterministic scan result. InputSHA256 binds it to one whole
// source file.
type Index struct {
	Version     int     `json:"version"`
	InputSHA256 string  `json:"input_sha256"`
	SourceSize  int64   `json:"source_size"`
	Fragmented  bool    `json:"fragmented"`
	Tracks      []Track `json:"tracks"`
}

func (idx *Index) Track(id uint32) (*Track, bool) {
	for i := range idx.Tracks {
		if idx.Tracks[i].ID == id {
			return &idx.Tracks[i], true
		}
	}
	return nil, false
}

func corruptBox(source string, b *BoxHeader, format string, args ...any) *model.Error {
	msg := fmt.Sprintf(format, args...)
	e := model.New(model.CodeCorruptBox, model.StageScan, msg).WithSource(source)
	if b != nil {
		e.WithBox(b.Type).WithOffset(b.Offset)
	}
	return e
}
