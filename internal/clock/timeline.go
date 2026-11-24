package clock

import (
	"bcast/internal/boxes"
	"bcast/internal/media"
)

// RepairedSample retains the original trun fields verbatim and adds monotonic
// decode and composition timestamps in both track and shared time bases.
type RepairedSample struct {
	media.SampleTiming
	DTS       int64 `json:"dts"`
	PTS       int64 `json:"pts"`
	SharedDTS int64 `json:"shared_dts"`
	SharedPTS int64 `json:"shared_pts"`
}

// FragmentTimeline is the repaired timeline for one unique content fragment.
type FragmentTimeline struct {
	OriginalFragment uint32           `json:"original_fragment"`
	FileOrdinal      int              `json:"file_ordinal"`
	TrackID          uint32           `json:"track_id"`
	Sequence         uint32           `json:"sequence"`
	Path             string           `json:"path"`
	MoofOffset       int64            `json:"moof_offset"`
	ContentSHA256    string           `json:"content_sha256"`
	Timescale        uint32           `json:"timescale"`
	StartDTS         int64            `json:"start_dts"`
	EndDTS           int64            `json:"end_dts"`
	StartShared      int64            `json:"start_shared"`
	EndShared        int64            `json:"end_shared"`
	Samples          []RepairedSample `json:"samples"`
	SyncIndexes      []int            `json:"sync_indexes"`
	Actions          []string         `json:"actions,omitempty"`
}

// TrackTimeline contains one track's repaired fragments and drift estimate.
type TrackTimeline struct {
	TrackID     uint32              `json:"track_id"`
	Timescale   uint32              `json:"timescale"`
	Fragments   []*FragmentTimeline `json:"fragments"`
	StartShared int64               `json:"start_shared"`
	EndShared   int64               `json:"end_shared"`
	DriftPPM    float64             `json:"drift_ppm"`
}

// Result is the shared-time-base repair result.
type Result struct {
	SharedBase media.Rational            `json:"shared_base"`
	Tracks     map[uint32]*TrackTimeline `json:"tracks"`
	Fragments  []*FragmentTimeline       `json:"fragments"`
}

// RepairOptions controls strictness.
type RepairOptions struct {
	// AllowAnchorlessClamp permits a regressed tfdt to be replaced by the
	// previous fragment end when no RTP anchor is available.
	AllowAnchorlessClamp bool
}

type trackBuilder struct {
	track     *TrackTimeline
	boxes     []*boxes.Fragment
	anchor    map[uint32]*Anchor
	rtpX      []int64
	rtpY      []int64
	unwrapped map[uint32]int64
}
