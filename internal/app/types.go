// Package app wires ingest, timeline, inspection, metadata, RTP
// windowing, repair, the bounded pipeline and remux into one run.
package app

import (
	"errors"
	"time"

	"mediacheck/internal/clock"
	"mediacheck/internal/fixture"
	"mediacheck/internal/inspect"
	"mediacheck/internal/model"
	"mediacheck/internal/remux"
)

// Options is the validated CLI configuration.
type Options struct {
	Profile    fixture.Profile
	LossBurst  int
	SlowSinkMs int
	MemoryMiB  int64
	DurationMs int64
	WindowCap  int
	MaxKfGapMs int64
	DriftMs    int64
}

// DefaultOptions returns the documented broadcast defaults.
func DefaultOptions() Options {
	return Options{
		Profile: fixture.ProfileBroadcast, LossBurst: 12, SlowSinkMs: 20,
		MemoryMiB: 32, DurationMs: 2000, WindowCap: 64,
		MaxKfGapMs: 2500, DriftMs: 0,
	}
}

// Item is the unit moving through the bounded pipeline.
type Item struct {
	Track       string
	StartMs     int64
	EndMs       int64
	Bytes       int
	Sample      inspect.Sample
	EnqueuedAt  time.Time
}

// Result bundles everything the CLI reports.
type Result struct {
	Summary   remux.Summary
	Faults    []model.Fault
	Segments  []model.Segment
	Exit      model.ExitCode
	Output    []byte
	Committed []model.CommittedBoundary
}

func validate(o Options) error {
	if o.MemoryMiB <= 0 {
		return errors.New("memory-limit-mib must be positive")
	}
	if o.WindowCap <= 0 || o.MaxKfGapMs <= 0 || o.DurationMs <= 0 {
		return errors.New("window, keyframe gap and duration must be positive")
	}
	return nil
}

// ValidateForCLI exposes option validation for the command entry point.
func ValidateForCLI(o Options) error { return validate(o) }

// faultIDGen returns a closure issuing stable sequential fault IDs.
func faultIDGen(faults *[]model.Fault) func() int {
	next := 1
	for _, f := range *faults {
		if f.ID >= next {
			next = f.ID + 1
		}
	}
	return func() int {
		id := next
		next++
		return id
	}
}

// maxDrift computes the largest genuine A/V clock displacement in ms.
// Healthy tracks are generated on the same ms grid so this returns 0;
// only an injected drift (or a non-zero start offset) moves it.
func maxDrift(pkg *fixture.Package) int64 {
	v, vok := pkg.Samples["video"]
	a, aok := pkg.Samples["audio"]
	if !vok || !aok || len(v) == 0 || len(a) == 0 {
		return 0
	}
	vStart := v[0].StartMs
	aStart := a[0].StartMs
	base := clock.Drift(vStart, aStart)
	vEnd := v[len(v)-1].StartMs + v[len(v)-1].DurMs
	aEnd := a[len(a)-1].StartMs + a[len(a)-1].DurMs
	end := clock.Drift(vEnd, aEnd)
	if end > base {
		return end
	}
	return base
}
