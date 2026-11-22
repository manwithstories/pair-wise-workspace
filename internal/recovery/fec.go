// Package recovery contains the loss-recovery policy used by the timeline.
// The archive profile never fabricates precise timing: a gap is either bounded
// by the next observed packet and filled with labelled silence, or it is
// declared unrecoverable.
package recovery

import (
	"fmt"

	"example.com/medialab/archive/internal/model"
)

// Action names the recovery operation recorded in provenance.
type Action string

const (
	ActionSilenceFill   Action = "silence-fill"
	ActionUnrecoverable Action = "unrecoverable"
)

// Evidence describes one gap. Microseconds throughout.
type Evidence struct {
	HoleSeq       uint32
	HoleStartUS   int64
	PreviousEndUS int64
	NextStartUS   int64
	HasNext       bool
	MaxFillUS     int64
	Reason        model.ErrorKind
}

// Decision is the deterministic outcome for one gap.
type Decision struct {
	Recoverable bool
	Action      Action
	StartUS     int64
	EndUS       int64
	DurationUS  int64
	Estimated   bool
	Detail      string
	// Reason carries the gap classification (loss vs corruption) so callers
	// can report each cause separately.
	Reason model.ErrorKind
}

// Decide applies the silence-fill policy. It is pure and deterministic so a
// replay of the same gaps produces byte-identical recoveries.
func Decide(e Evidence) Decision {
	if !e.HasNext {
		return Decision{
			Action:    ActionUnrecoverable,
			StartUS:   e.HoleStartUS,
			Detail:    "no following packet bounds the gap duration",
			Estimated: false,
		}
	}
	start := e.HoleStartUS
	if start < e.PreviousEndUS {
		start = e.PreviousEndUS
	}
	dur := e.NextStartUS - start
	if dur <= 0 {
		return Decision{
			Action:    ActionUnrecoverable,
			StartUS:   start,
			Detail:    fmt.Sprintf("non-positive inferred gap %d us", dur),
			Estimated: false,
			Reason:    e.Reason,
		}
	}
	if e.MaxFillUS > 0 && dur > e.MaxFillUS {
		return Decision{
			Action:    ActionUnrecoverable,
			StartUS:   start,
			Detail:    fmt.Sprintf("gap %d us exceeds fill limit %d us", dur, e.MaxFillUS),
			Estimated: true,
			Reason:    e.Reason,
		}
	}
	return Decision{
		Recoverable: true,
		Action:      ActionSilenceFill,
		StartUS:     start,
		EndUS:       start + dur,
		DurationUS:  dur,
		Estimated:   true,
		Detail:      fmt.Sprintf("silence fill for %s gap", reasonLabel(e.Reason)),
		Reason:      e.Reason,
	}
}

func reasonLabel(k model.ErrorKind) string {
	if k == model.KindNone {
		return "missing-packet"
	}
	return string(k)
}

// Stats aggregates recovery outcomes for reporting and checkpoints. Loss and
// corruption are kept apart so the recovery rate measures packet loss only.
type Stats struct {
	LossHoles        int64 `json:"loss_holes"`
	RecoveredLoss    int64 `json:"recovered_loss_holes"`
	CorruptHoles     int64 `json:"corrupt_holes"`
	RecoveredCorrupt int64 `json:"recovered_corrupt_holes"`
	Unrecoverable    int64 `json:"unrecoverable"`
	EstimatedUS      int64 `json:"estimated_us"`
}

// Rate returns the share of loss holes that were recoverable. Unrecoverable
// holes stay in the denominator: a gap that could not be proven is a loss the
// archive did not recover. A run without loss is trivially at 100%.
func (s Stats) Rate() float64 {
	denom := s.LossHoles + s.Unrecoverable
	if denom == 0 {
		return 1
	}
	return float64(s.RecoveredLoss) / float64(denom)
}
