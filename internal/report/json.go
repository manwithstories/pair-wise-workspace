// Package report renders the machine-readable result of a run. It contains
// only measurements, classifications and error descriptions: never raw audio
// samples, packet payloads or file contents.
package report

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"

	"avpipe/internal/fault"
)

// Thresholds are the acceptance limits for one run class. A zero value means
// the corresponding metric is not checked.
type Thresholds struct {
	SyncP95Ms       float64 `json:"sync_p95_ms,omitempty"`
	LatencyP95Ms    float64 `json:"latency_p95_ms,omitempty"`
	RecoveryRateMin float64 `json:"packet_recovery_rate_min,omitempty"`
	QueuePeakMax    int64   `json:"queue_peak_bytes_max,omitempty"`
}

// Metric is one measured value with its verdict.
type Metric struct {
	Name    string  `json:"name"`
	Value   float64 `json:"value"`
	Limit   float64 `json:"limit,omitempty"`
	Passed  bool    `json:"passed"`
	Checked bool    `json:"checked"`
}

// Check evaluates value against limit using "max" or "min" comparison.
func (t Thresholds) Check(name string, value, limit float64, atMost bool) Metric {
	m := Metric{Name: name, Value: value, Limit: limit, Passed: true, Checked: limit != 0}
	if m.Checked {
		if atMost {
			m.Passed = value <= limit
		} else {
			m.Passed = value >= limit
		}
	}
	return m
}

// ErrorInfo is the serializable form of the unified error.
type ErrorInfo struct {
	Code   string `json:"code"`
	Op     string `json:"op"`
	Track  string `json:"track,omitempty"`
	Offset *int64 `json:"offset,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// FromError converts an error to its report form using the root cause.
func FromError(err error) *ErrorInfo {
	root := fault.Root(err)
	if root == nil {
		return nil
	}
	info := &ErrorInfo{Code: string(root.Code), Op: root.Op, Track: root.Track,
		Detail: root.Detail}
	if root.HasOffset {
		off := root.Offset
		info.Offset = &off
	}
	return info
}

// Report is the JSON document written by run and selftest.
type Report struct {
	Command          string        `json:"command"`
	Profile          string        `json:"profile"`
	Seed             uint64        `json:"seed"`
	SampleRate       int           `json:"sample_rate"`
	Layout           string        `json:"layout"`
	TargetLUFS       float64       `json:"target_lufs"`
	SyncErrorMs      float64       `json:"sync_error_ms"`
	LatencyP95Ms     float64       `json:"latency_p95_ms"`
	PacketRecovery   float64       `json:"packet_recovery_rate"`
	QueuePeakBytes   int64         `json:"queue_peak_bytes"`
	QueuePeakTicks   int64         `json:"queue_peak_ticks"`
	RMSBefore        float64       `json:"rms_before"`
	RMSAfter         float64       `json:"rms_after"`
	PeakBefore       float64       `json:"peak_before"`
	PeakAfter        float64       `json:"peak_after"`
	GainDB           float64       `json:"gain_db"`
	RoundingError    float64       `json:"resample_rounding_error"`
	Resamples        int64         `json:"resampled_frames"`
	Frames           int           `json:"frames"`
	Blocks           int           `json:"blocks"`
	SilenceFills     int           `json:"silence_fills"`
	Consumed         int64         `json:"blocks_consumed"`
	Dropped          int64         `json:"blocks_dropped"`
	Evicted          int64         `json:"blocks_evicted"`
	RTPLost          int           `json:"rtp_lost"`
	RTPRecovered     int           `json:"rtp_recovered"`
	RTPUnrecoverable int           `json:"rtp_unrecoverable"`
	RTPConflicts     int           `json:"rtp_conflicts"`
	DriftEstimatePPM float64       `json:"drift_estimate_ppm"`
	DriftResolvable  bool          `json:"drift_resolvable"`
	DriftStdErrPPM   float64       `json:"drift_stderr_ppm"`
	SyncP95BeforeMs  float64       `json:"sync_p95_before_ms"`
	LatencySamples   int           `json:"latency_samples"`
	Tracks           []TrackStatus `json:"tracks,omitempty"`
	Scenarios        []Scenario    `json:"scenarios,omitempty"`
	Metrics          []Metric      `json:"metrics,omitempty"`
	RootError        *ErrorInfo    `json:"root_error"`
	Errors           []ErrorInfo   `json:"errors,omitempty"`
	MP4Clip          string        `json:"mp4_clip,omitempty"`
	Passed           bool          `json:"passed"`
	ExitCode         int           `json:"exit_code"`
}

// TrackStatus is the per-track result of the dependency run.
type TrackStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Scenario is one isolated fault-path result printed by selftest.
type Scenario struct {
	Name     string     `json:"name"`
	Passed   bool       `json:"passed"`
	Detail   string     `json:"detail"`
	Root     *ErrorInfo `json:"root_error,omitempty"`
	ExitCode int        `json:"exit_code"`
}

// Finalize evaluates metrics, sets Passed and ExitCode, and derives the
// root_error field. Any failed metric maps to exit code 3; an unhandled
// internal error maps to 4; everything else is 0.
func (r *Report) Finalize() {
	r.Passed = true
	for _, m := range r.Metrics {
		if m.Checked && !m.Passed {
			r.Passed = false
			if r.ExitCode == 0 {
				r.ExitCode = 3
			}
		}
	}
	if r.RootError != nil {
		code := fault.ExitCode(fault.New(fault.Code(r.RootError.Code), r.RootError.Op, "%s", r.RootError.Detail))
		if code == 4 {
			r.ExitCode = 4
			r.Passed = false
		}
	}
}

// Sanitize replaces non-finite float values with 0 so a degenerate run
// (for example a silent stream whose LUFS is -Inf) still produces a valid
// JSON document instead of failing at encode time.
func (r *Report) Sanitize() {
	finite := func(v *float64) {
		if math.IsNaN(*v) || math.IsInf(*v, 0) {
			*v = 0
		}
	}
	for _, f := range []*float64{
		&r.TargetLUFS, &r.SyncErrorMs, &r.LatencyP95Ms, &r.PacketRecovery,
		&r.RMSBefore, &r.RMSAfter, &r.PeakBefore, &r.PeakAfter, &r.GainDB,
		&r.RoundingError, &r.DriftEstimatePPM, &r.DriftStdErrPPM,
		&r.SyncP95BeforeMs,
	} {
		finite(f)
	}
	for i := range r.Metrics {
		finite(&r.Metrics[i].Value)
		finite(&r.Metrics[i].Limit)
	}
}

// Encode renders the report as indented JSON.
func (r *Report) Encode() ([]byte, error) {
	r.Sanitize()
	buf, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fault.Wrap(fault.CodeInternal, "report.encode", err, "marshal report")
	}
	return append(buf, '\n'), nil
}

// WriteEncode writes the report to path, creating the parent directory. A
// temporary file is renamed into place so a partial report never survives.
func (r *Report) WriteEncode(path string) error {
	buf, err := r.Encode()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fault.Wrap(fault.CodeInternal, "report.write", err, "create %s", dir)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return fault.Wrap(fault.CodeInternal, "report.write", err, "write %s", tmp)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fault.Wrap(fault.CodeInternal, "report.write", err, "rename to %s", path)
	}
	return nil
}
