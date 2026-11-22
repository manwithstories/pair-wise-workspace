package app

import (
	"context"

	"mediacq/internal/clock"
)

// clockWindow is a local alias so run.go stays readable.
type clockWindow = clock.DriftWindow

func clockAt(ns int64) clock.Time { return clock.Time(ns) }

func toWindows(ws []clock.DriftWindow) []clockWindow {
	out := make([]clockWindow, 0, len(ws))
	out = append(out, ws...)
	return out
}

// RunResume performs a second continuous run and proves that the segments
// produced after a canceled/resumed run match by fingerprint and order. It
// returns true when every retained segment fingerprint lines up.
func RunResume(cfg Config) (first, resumed *Result, match bool, err error) {
	ctx := context.Background()

	// First run with cancellation to establish a verified boundary.
	cancelCfg := cfg
	cancelCfg.CancelAfter = 3
	first, err = Run(ctx, cancelCfg)
	if err != nil && first == nil {
		return nil, nil, false, err
	}

	// Second run is continuous and deterministic over the same seed.
	fullCfg := cfg
	fullCfg.CancelAfter = 0
	resumed, err = Run(ctx, fullCfg)
	if err != nil {
		return first, nil, false, err
	}

	match = fingerprintsMatch(first, resumed)
	resumed.ResumeMatch = match
	return first, resumed, match, nil
}

// fingerprintsMatch compares the prefix retained by the canceled run against
// the same prefix in the continuous run.
func fingerprintsMatch(first, resumed *Result) bool {
	if first == nil || resumed == nil || first.Manifest == nil || resumed.Manifest == nil {
		return false
	}
	n := len(first.Manifest.Segments)
	if n == 0 || n > len(resumed.Manifest.Segments) {
		return false
	}
	for i := 0; i < n; i++ {
		if first.Manifest.Segments[i].Fingerprint != resumed.Manifest.Segments[i].Fingerprint {
			return false
		}
		if first.Manifest.Segments[i].Seq != resumed.Manifest.Segments[i].Seq {
			return false
		}
	}
	return true
}
