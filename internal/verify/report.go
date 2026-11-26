package verify

import (
	"encoding/json"

	"bq/internal/model"
)

// Report is the full evidence document written to disk and printed as JSON.
type Report struct {
	Status             string        `json:"status"`
	Metrics            Metrics       `json:"metrics"`
	FailedGates        []string      `json:"failed_gates"`
	Errors             []model.Error `json:"errors"`
	Propagation        Propagation   `json:"propagation"`
	WrapEvents         int           `json:"wrap_events"`
	UnrecoverableRanges int          `json:"unrecoverable_ranges"`
}

// Propagation proves anomalies reached the verifier instead of being
// silently swallowed by an upstream stage.
type Propagation struct {
	WrapObserved      bool `json:"wrap_observed"`
	LossObserved      bool `json:"loss_observed"`
	DuplicateObserved bool `json:"duplicate_observed"`
	TruncationObserved bool `json:"truncation_observed"`
	AnchorConflicts   bool `json:"anchor_conflicts"`
}

// BuildReport assembles the evidence document from metrics and sorted errors.
func BuildReport(m Metrics, errs []model.Error, th Thresholds,
	wrapEvents, unrecoverableRanges int) Report {

	rep := Report{
		Metrics:             m,
		FailedGates:         m.Evaluate(th),
		Errors:              errs,
		WrapEvents:          wrapEvents,
		UnrecoverableRanges: unrecoverableRanges,
		Propagation:         detectPropagation(errs),
	}
	switch {
	case model.HasFatal(errs):
		rep.Status = "unrecoverable_input"
	case len(rep.FailedGates) > 0:
		rep.Status = "threshold_exceeded"
	default:
		rep.Status = "pass"
	}
	return rep
}

func detectPropagation(errs []model.Error) Propagation {
	var p Propagation
	for _, e := range errs {
		switch e.Code {
		case model.ErrPTSWrap:
			p.WrapObserved = true
		case model.ErrPacketLoss, model.ErrPacketUnrecovered:
			p.LossObserved = true
		case model.ErrDuplicatePacket:
			p.DuplicateObserved = true
		case model.ErrTailTruncation:
			p.TruncationObserved = true
		case model.ErrAnchorConflict:
			p.AnchorConflicts = true
		}
	}
	return p
}

// JSON renders the report with stable key ordering (struct order) so the
// output is byte-comparable across repeated runs.
func (r Report) JSON() ([]byte, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// ExitCode maps the report status onto the documented process exit codes:
// 0 pass, 1 metric threshold exceeded, 2 unrecoverable input, 64 bad args
// (handled by the CLI before a report exists).
func (r Report) ExitCode() int {
	switch r.Status {
	case "pass":
		return 0
	case "threshold_exceeded":
		return 1
	case "unrecoverable_input":
		return 2
	default:
		return 1
	}
}
