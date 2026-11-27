// Package report measures the packaged output (bitrate, frames, black,
// loudness, cross-rendition alignment, event coverage) and evaluates the
// broadcast quality gates.
package report

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"aircheck/internal/fault"
)

// Quality gates.
const (
	MaxDriftPct    = 8.0
	MaxAlignmentMs = 20.0
)

// RenditionStats holds measured metrics for one output rendition.
type RenditionStats struct {
	Name          string  `json:"name"`
	TargetBPS     int64   `json:"target_bps"`
	ActualBPS     int64   `json:"actual_bps"`
	DriftPct      float64 `json:"drift_pct"`
	Frames        int64   `json:"frames"`
	BlackPct      float64 `json:"black_pct"`
	AudioRMSDbFS  float64 `json:"audio_rms_dbfs"`
	AudioPeakDbFS float64 `json:"audio_peak_dbfs"`
}

// Report is the audit output document.
type Report struct {
	Package        string           `json:"package"`
	OK             bool             `json:"ok"`
	Renditions     []RenditionStats `json:"renditions"`
	MaxAlignmentMs float64          `json:"max_alignment_ms"`
	EventsTotal    int              `json:"events_total"`
	EventMissing   int              `json:"event_missing"`
	Faults         []fault.Error    `json:"faults,omitempty"`
	Digest         string           `json:"digest"`
}

// Finalize evaluates quality gates, collects faults and computes the
// deterministic digest of the report (excluding the digest itself).
func (r *Report) Finalize() {
	r.OK = true
	for _, rs := range r.Renditions {
		if rs.DriftPct > MaxDriftPct {
			r.addFault(fault.Error{Code: fault.CodeDriftExceeded, Track: rs.Name, Tick: -1,
				Msg: fmt.Sprintf("drift %.2f%% exceeds %.0f%%", rs.DriftPct, MaxDriftPct)})
		}
		if rs.Frames <= 0 {
			r.addFault(fault.Error{Code: fault.CodeMissingTrack, Track: rs.Name, Tick: -1,
				Msg: "zero decoded frames"})
		}
	}
	if r.MaxAlignmentMs > MaxAlignmentMs {
		r.addFault(fault.Error{Code: fault.CodeAlignExceeded, Tick: -1,
			Msg: fmt.Sprintf("alignment %.2f ms exceeds %.0f ms", r.MaxAlignmentMs, MaxAlignmentMs)})
	}
	if r.EventsTotal == 0 {
		r.addFault(fault.Error{Code: fault.CodeEventMissing, Tick: -1, Msg: "no timed events indexed"})
	}
	if r.EventMissing > 0 {
		r.addFault(fault.Error{Code: fault.CodeEventMissing, Tick: -1,
			Msg: fmt.Sprintf("%d events not covered by output timeline", r.EventMissing)})
	}
	r.Digest = r.computeDigest()
}

func (r *Report) addFault(f fault.Error) {
	r.OK = false
	r.Faults = append(r.Faults, f)
}

func (r *Report) computeDigest() string {
	c := *r
	c.Digest = ""
	raw, _ := json.Marshal(c)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}
