// Package pipeline wires adapters, reordering, alignment and manifest
// generation into one deterministic run, with checkpointing so a restarted
// process reproduces the uninterrupted result segment for segment.
package pipeline

import (
	"mediaroute/internal/adapter"
	"mediaroute/internal/align"
	"mediaroute/internal/fault"
	"mediaroute/internal/manifest"
	"mediaroute/internal/reorder"
	"mediaroute/internal/sim"
)

// DefaultWindowSize bounds the reorder window (and the recoverable burst).
const DefaultWindowSize = 8

// Engine processes one stream. Its whole state is checkpointable.
type Engine struct {
	reg       *adapter.Registry
	rtp       *adapter.RTPParser
	windowSz  int
	windows   map[string]*reorder.Window
	rec       *reorder.Recovery
	delivered []adapter.Event
	isolated  map[string]bool

	// Errs collects every classified fault of the run.
	Errs *fault.List
	// RollbackItems reports how many input items were re-processed after
	// the last checkpoint recovery.
	RollbackItems int
}

// NewEngine wires the adapter registry with the built-in parsers.
func NewEngine(windowSize int) *Engine {
	if windowSize <= 0 {
		windowSize = DefaultWindowSize
	}
	rtpP := adapter.NewRTPParser(sim.ClockRate, sim.BaseTick)
	reg := adapter.NewRegistry()
	reg.Register("rtp", rtpP)
	reg.Register("mp4", adapter.NewMP4Parser())
	reg.Register("sei", adapter.NewSEIParser())
	return &Engine{
		reg: reg, rtp: rtpP, windowSz: windowSize,
		windows:  map[string]*reorder.Window{},
		rec:      &reorder.Recovery{},
		isolated: map[string]bool{},
		Errs:     &fault.List{},
	}
}

// Step processes one input item. Parse and version faults isolate the
// offending track without affecting independent tracks.
func (e *Engine) Step(it sim.Item) {
	if e.isolated[it.Track] {
		return
	}
	events, err := e.reg.Parse(it.Parser, it.Track, it.Data, it.Offset)
	if err != nil {
		e.Errs.Add(err)
		e.isolated[it.Track] = true
		return
	}
	for _, ev := range events {
		if ev.Kind != adapter.KindMedia {
			e.delivered = append(e.delivered, ev)
			continue
		}
		w := e.windows[ev.Track]
		if w == nil {
			w = reorder.NewWindow(e.windowSz)
			e.windows[ev.Track] = w
		}
		prevGaps := len(w.Gaps)
		out := w.Push(ev)
		e.rec.RecordAll(ev.Track, w.Gaps[prevGaps:])
		e.delivered = append(e.delivered, out...)
	}
}

// Result is the outcome of one pipeline run.
type Result struct {
	Manifest          *manifest.Manifest
	SegEvents         [][]adapter.Event
	Boundaries        []align.Boundary
	Errs              *fault.List
	DuplicatesDropped int
	LossBurstRecovered int
	IsolatedTracks    []string
	RollbackItems     int
	AlignP95Ns        int64
	PublishRejected   bool
}

// Finish flushes windows, validates events, aligns boundaries and builds
// the manifest.
func (e *Engine) Finish(s *sim.Stream) *Result {
	for track, w := range e.windows {
		prev := len(w.Gaps)
		e.delivered = append(e.delivered, w.Flush()...)
		e.rec.RecordAll(track, w.Gaps[prev:])
	}
	for _, fe := range e.rec.Faults() {
		e.Errs.Add(fe)
	}
	events := append(append([]adapter.Event(nil), e.delivered...), s.Meta...)
	valid, timingErrs := align.Validate(events)
	for _, fe := range timingErrs {
		e.Errs.Add(fe)
	}
	targets := Targets(s.DurationNs, s.Bandwidth)
	bounds, alignErrs := align.Align(targets, s.Keyframes, sim.PrimaryRendition, 500_000_000)
	rejected := false
	for _, fe := range alignErrs {
		e.Errs.Add(fe)
		rejected = true
	}
	bl := make([]int64, 0, len(bounds)+1)
	var errs []int64
	for _, b := range bounds {
		errs = append(errs, b.ErrorNs)
		if len(bl) == 0 || b.ChosenNs > bl[len(bl)-1] {
			bl = append(bl, b.ChosenNs)
		}
	}
	bl = append(bl, s.DurationNs)
	segEvents, _ := align.Project(valid, bl)
	m := manifest.Build(bl, segEvents, s.Keyframes[sim.PrimaryRendition])
	res := &Result{
		Manifest: m, SegEvents: segEvents, Boundaries: bounds,
		Errs:               e.Errs,
		LossBurstRecovered: e.rec.BurstsRecovered,
		RollbackItems:      e.RollbackItems,
		AlignP95Ns:         align.P95(errs),
		PublishRejected:    rejected,
	}
	for _, w := range e.windows {
		res.DuplicatesDropped += w.Duplicates
	}
	for track := range e.isolated {
		res.IsolatedTracks = append(res.IsolatedTracks, track)
	}
	return res
}

// Targets computes segment boundary target times from bandwidth steps:
// each equal-length phase uses the target duration for its bandwidth.
func Targets(durationNs int64, bandwidth []int64) []int64 {
	if len(bandwidth) == 0 {
		bandwidth = []int64{2_000_000}
	}
	phase := durationNs / int64(len(bandwidth))
	if phase <= 0 {
		phase = 1
	}
	var targets []int64
	// A target closer than the minimum step to the end would produce a
	// sliver segment with no keyframe; merge it into the previous one.
	for t := int64(0); t+1_000_000_000 <= durationNs; {
		targets = append(targets, t)
		i := int(t / phase)
		if i >= len(bandwidth) {
			i = len(bandwidth) - 1
		}
		t += targetDuration(bandwidth[i])
	}
	return targets
}

// targetDuration scales a 2s base duration inversely to bandwidth,
// clamped to [1s, 4s].
func targetDuration(bw int64) int64 {
	if bw <= 0 {
		bw = 1
	}
	d := 2_000_000_000 * 2_000_000 / bw
	if d < 1_000_000_000 {
		d = 1_000_000_000
	}
	if d > 4_000_000_000 {
		d = 4_000_000_000
	}
	return d
}
