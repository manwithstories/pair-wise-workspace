// Package recover turns scanned media into a recovery plan. Tracks are
// processed in dependency order: a track's clock must be normalized and its
// keyframes verified before any clip boundary that depends on it is recovered.
// A track that cannot be safely bounded blocks its successors but leaves
// independent branches untouched.
package recover

import (
	"archivearchivist/internal/clock"
	"archivearchivist/internal/graph"
	"archivearchivist/internal/model"
	"archivearchivist/internal/mp4"
	"archivearchivist/internal/rtp"
)

// Config tunes boundary recovery.
type Config struct {
	// KeyframeSearch is how many samples on each side of a damaged run are
	// inspected for a decodable keyframe.
	KeyframeSearch int
	// MaxErrorUS is the largest acceptable distance between an intended cut
	// and the keyframe it must snap to.
	MaxErrorUS int64
}

// DefaultConfig returns the acceptance defaults.
func DefaultConfig() Config {
	return Config{KeyframeSearch: 120, MaxErrorUS: 40_000}
}

// Range is the recovery verdict for one contiguous damaged byte interval.
type Range struct {
	Track        string `json:"track"`
	CorruptStart int64  `json:"corrupt_start"`
	CorruptEnd   int64  `json:"corrupt_end"`
	// BeforeKeyframe and AfterKeyframe are the original byte offsets of the
	// decodable keyframes bracketing the damage, or -1 when absent.
	BeforeKeyframe int64 `json:"before_keyframe"`
	AfterKeyframe  int64 `json:"after_keyframe"`
	// RecoverableStart/End is the byte interval that can be rebuilt by
	// decoding from the bracketing keyframes.
	RecoverableStart int64  `json:"recoverable_start"`
	RecoverableEnd   int64  `json:"recoverable_end"`
	ErrorBoundUS     int64  `json:"error_bound_us"`
	Status           string `json:"status"` // "recovered" or "unrecoverable"
	Detail           string `json:"detail,omitempty"`
}

// Boundary is one recovered clip edge.
type Boundary struct {
	Clip       string   `json:"clip"`
	Track      string   `json:"track"`
	Edge       string   `json:"edge"` // "in" or "out"
	Intended   Rational `json:"intended"`
	Keyframe   int64    `json:"keyframe_index"`
	MediaTicks int64    `json:"media_ticks"`
	MediaTime  Rational `json:"media_time"`
	ErrorUS    int64    `json:"error_us"`
	Snapped    bool     `json:"snapped"`
	Status     string   `json:"status"` // "recovered", "blocked", "out_of_range"
}

// Rational aliases the model type so JSON stays stable.
type Rational = model.Rational

// TrackState records what the planner decided about one track.
type TrackState struct {
	Track     string   `json:"track"`
	OK        bool     `json:"ok"`
	Reason    string   `json:"reason,omitempty"`
	ClockNorm bool     `json:"clock_normalized"`
	Keyframes int      `json:"verified_keyframes"`
	BlockedBy string   `json:"blocked_by,omitempty"`
	DriftPPM  Rational `json:"drift_ppm"`
	Wraps     int      `json:"wraps"`
	VFR       bool     `json:"variable_frame_rate"`
	Isolated  int      `json:"isolated_samples"`
}

// TransitionResult is the sync verdict at a crossfade.
type TransitionResult struct {
	ID     string `json:"id"`
	From   string `json:"from"`
	To     string `json:"to"`
	SkewUS int64  `json:"skew_us"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Result is the whole recovery plan.
type Result struct {
	Ranges      []Range            `json:"ranges"`
	Boundaries  []Boundary         `json:"boundaries"`
	Transitions []TransitionResult `json:"transitions"`
	Tracks      []TrackState       `json:"tracks"`
	Faults      model.FaultList    `json:"faults"`
	Recovered   bool               `json:"recovered"`
}

// Input is everything the planner needs, already parsed.
type Input struct {
	Timeline *model.Timeline
	Scans    map[string]*mp4.Scan // keyed by timeline track ID
	Capture  *rtp.Report
	Clocks   map[string]clock.Result
	Graph    *graph.Graph
}
