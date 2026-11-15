// Package report judges measurements and renders human/JSON output.
package report

import (
	"math"

	"loudness/internal/loudness"
)

const DefaultToleranceLU = 0.5

const (
	VerdictPass = "PASS"
	VerdictFail = "FAIL"

	ReasonBelowTarget      = "below_target"
	ReasonAboveTarget      = "above_target"
	ReasonTruePeakExceeded = "true_peak_exceeded"
	ReasonNonFiniteSamples = "non_finite_samples"
)

// Row is one measurement or one batch error item.
type Row struct {
	File    string
	Metrics loudness.Metrics
	Verdict string
	Reason  string
	Err     string
}

// Judge applies the delivery target and ceiling. The absolute target
// tolerance is one half LU by default.
func Judge(file string, m loudness.Metrics, target, ceiling, tolerance float64) Row {
	r := Row{File: file, Metrics: m, Verdict: VerdictPass}
	if m.Reason != "" {
		r.Verdict = VerdictFail
		r.Reason = m.Reason
		return r
	}
	if m.NaNs > 0 {
		r.Verdict = VerdictFail
		r.Reason = ReasonNonFiniteSamples
		return r
	}
	switch {
	case math.IsInf(m.IntegratedLUFS, -1) || m.IntegratedLUFS < target-tolerance:
		r.Verdict = VerdictFail
		r.Reason = ReasonBelowTarget
	case m.IntegratedLUFS > target+tolerance:
		r.Verdict = VerdictFail
		r.Reason = ReasonAboveTarget
	case !math.IsInf(m.TruePeakDBTP, -1) && m.TruePeakDBTP > ceiling:
		r.Verdict = VerdictFail
		r.Reason = ReasonTruePeakExceeded
	}
	return r
}

// ErrorRow records a parse/decode/format failure without stopping a batch.
func ErrorRow(file string, err error) Row {
	return Row{File: file, Verdict: "ERROR", Err: err.Error()}
}
