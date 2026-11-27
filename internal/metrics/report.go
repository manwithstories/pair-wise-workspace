// Package metrics aggregates delivery metrics and enforces their accepted
// bounds. A violation is a distinct error kind so the CLI can exit with 4.
package metrics

import (
	"errors"
	"fmt"
	"sort"

	"fieldcut/internal/media"
)

// Limits are the accepted delivery bounds.
type Limits struct {
	MaxSyncMs     float64
	MinRecovery   float64
	MaxP95FrameMs float64
}

// DefaultLimits are the documented demo bounds.
var DefaultLimits = Limits{MaxSyncMs: 2.0, MinRecovery: 0.95, MaxP95FrameMs: 100}

// Report is the terminal summary.
type Report struct {
	SyncMaxMs    float64
	RecoveryRate float64
	Eligible     int
	Recovered    int
	P95FrameMs   float64
	Frames       int
	RootSHA256   string
	ResumeEqual  bool
	Verify       string
}

// SyncMax samples are per-frame alignment errors in milliseconds.
func SyncMax(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	m := samples[0]
	for _, v := range samples[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

// Percentile returns the p-th percentile (0..100) with linear interpolation
// between order statistics; p95 of 0..99 is deterministic and exact enough
// for a bound check.
func Percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	if p <= 0 {
		return values[0]
	}
	if p >= 100 {
		max := values[0]
		for _, v := range values[1:] {
			if v > max {
				max = v
			}
		}
		return max
	}
	cp := append([]float64(nil), values...)
	sort.Float64s(cp)
	rank := p / 100 * float64(len(cp)-1)
	lo := int(rank)
	hi := lo + 1
	if hi >= len(cp) {
		return cp[len(cp)-1]
	}
	frac := rank - float64(lo)
	return cp[lo]*(1-frac) + cp[hi]*frac
}

// Check validates the report and returns a KindMetric error naming the
// first violated bound.
func (r Report) Check(l Limits) error {
	if r.SyncMaxMs < 0 || r.SyncMaxMs > l.MaxSyncMs {
		return media.Newf(media.KindMetric, "metrics.sync", "", -1,
			"sync_max_ms=%.2f outside [0,%.2f]", r.SyncMaxMs, l.MaxSyncMs)
	}
	if r.RecoveryRate < l.MinRecovery-1e-9 || r.RecoveryRate > 1.0000001 {
		return media.Newf(media.KindMetric, "metrics.recovery", "", -1,
			"recovery_rate=%.4f below %.2f", r.RecoveryRate, l.MinRecovery)
	}
	if r.P95FrameMs < 0 || r.P95FrameMs > l.MaxP95FrameMs {
		return media.Newf(media.KindMetric, "metrics.p95", "", -1,
			"p95_frame_ms=%.2f outside [0,%.0f]", r.P95FrameMs, l.MaxP95FrameMs)
	}
	if r.Frames <= 0 {
		return media.Newf(media.KindMetric, "metrics.frames", "", -1, "frames=%d", r.Frames)
	}
	if !r.ResumeEqual {
		return media.New(media.KindMetric, "metrics.resume", "", -1, errors.New("resume root hash differs from full run"))
	}
	if r.Verify != "PASS" {
		return media.New(media.KindMetric, "metrics.verify", "", -1, errors.New("verification failed"))
	}
	return nil
}

// Line renders the single-line key=value summary in stable field order.
func (r Report) Line() string {
	return fmt.Sprintf("sync_max_ms=%.1f recovery_rate=%.2f eligible=%d recovered=%d p95_frame_ms=%.0f frames=%d root_sha256=%s resume_equal=%t verify=%s",
		r.SyncMaxMs, r.RecoveryRate, r.Eligible, r.Recovered, r.P95FrameMs, r.Frames, r.RootSHA256, r.ResumeEqual, r.Verify)
}
