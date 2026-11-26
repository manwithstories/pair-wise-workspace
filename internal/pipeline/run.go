package pipeline

import (
	"sort"
	"time"

	"mediad/internal/av"
	"mediad/internal/clock"
	"mediad/internal/manifest"
	"mediad/internal/media"
	"mediad/internal/timeline"
)

// Options controls one delivery run.
type Options struct {
	MediaRoot string
	Manifest  *manifest.Manifest
	Edits     *manifest.EditList
	OutDir    string
	// LateWindow overrides the 2 s default (used by tests).
	LateWindow media.MediaTick
	// SnapshotStride tunes replay granularity.
	SnapshotStride int
	// Resume reads an existing checkpoint and skips completed segments.
	Resume bool
}

// Plan is a deterministic, fully-normalised description of the delivery. The
// same manifest and edits always produce the same plan, so repeated runs can
// be compared by hash.
type Plan struct {
	Segments []Segment
	Fits     map[string]*clock.Fit
	Drift    CameraDrift
	Events   []timeline.Event
	Late     int
	Replayed int
}

// Segment is one rendered edit, ordered on the shared timeline.
type Segment struct {
	Event timeline.Event
	Entry *manifest.Entry
	Path  string
	// StartTick/EndTick are the edit range mapped onto the host timeline.
	StartTick media.MediaTick
	EndTick   media.MediaTick
	// LengthSamples is the exact 48 kHz frame count.
	LengthSamples int
	AlignmentUS   float64
}

// CameraDrift is the worst absolute drift across cameras, with anomaly flag.
type CameraDrift struct {
	WorstPPM float64
	Anomaly  bool
	ByCamera map[string]float64
}

// BuildPlan normalises clocks, orders events and prepares segment metadata.
func BuildPlan(opts Options) (*Plan, error) {
	if opts.Manifest == nil || opts.Edits == nil {
		return nil, media.NewError(media.ErrManifest, "manifest and edits are required")
	}
	if err := opts.Edits.ValidateAgainst(opts.Manifest); err != nil {
		return nil, err
	}
	window := opts.LateWindow
	if window == 0 {
		window = timeline.LatenessWindow
	}
	stride := opts.SnapshotStride
	if stride == 0 {
		stride = 4
	}

	// Fit each camera's drift from its anchors.
	fits := map[string]*clock.Fit{}
	drift := CameraDrift{ByCamera: map[string]float64{}}
	for _, e := range opts.Manifest.Entries {
		if _, done := fits[e.Key()]; done {
			continue
		}
		anchors := make([]clock.Anchor, 0, len(e.Anchors))
		for _, a := range e.Anchors {
			anchors = append(anchors, clock.Anchor{HostUS: a.HostUS, MediaTicks: a.MediaTicks})
		}
		fit, err := clock.FitModel(anchors, e.TimeBase())
		if err != nil {
			return nil, err
		}
		fits[e.Key()] = fit
		drift.ByCamera[e.Camera] = fit.DriftPPM
		if absf(fit.DriftPPM) > absf(drift.WorstPPM) {
			drift.WorstPPM = fit.DriftPPM
		}
		if fit.Anomaly {
			drift.Anomaly = true
		}
	}

	// Build events for each edit and normalise their times.
	tl := timeline.New(window, stride)
	plan := &Plan{Fits: fits, Drift: drift}
	for _, edit := range opts.Edits.Edits {
		entry, ok := opts.Manifest.Lookup(edit.Camera, edit.Track)
		if !ok {
			return nil, media.NewError(media.ErrManifest, "edit %s/%s has no manifest entry", edit.Camera, edit.Track)
		}
		resolved, err := manifest.ResolveWithin(opts.MediaRoot, entry.File)
		if err != nil {
			return nil, err
		}
		fit := fits[entry.Key()]
		// Edits are expressed in the track's wrapped raw ticks. Lift each in
		// point to the unwrapped epoch nearest the sync anchors, so an edit
		// range that crosses the 33-bit boundary keeps its true ordering.
		refs := make([]int64, 0, len(entry.Anchors))
		for _, a := range entry.Anchors {
			refs = append(refs, a.MediaTicks)
		}
		unwrappedIn, err := clock.UnwrapNear(edit.In, entry.WrapBits, refs)
		if err != nil {
			return nil, err
		}
		// The out point is an exclusive bound; preserve the edit's duration by
		// offsetting from the unwrapped in point.
		unwrappedOut := unwrappedIn + edit.Duration()
		startTick := fit.ToTimeline(unwrappedIn, entry.TimeBase())
		endTick := fit.ToTimeline(unwrappedOut, entry.TimeBase())
		length, err := av.SpanSamples(entry.TimeBase(), unwrappedIn, unwrappedOut)
		if err != nil {
			return nil, err
		}
		alignUS, err := av.SpanAlignmentUS(entry.TimeBase(), unwrappedIn, unwrappedOut)
		if err != nil {
			return nil, err
		}
		// The asset a cut points into must not exceed the 12 hour ceiling; a
		// longer file is refused before any decoding starts.
		if secs := timeSpanSeconds(entry.TimeBase(), unwrappedIn, unwrappedOut); secs > MaxAssetSeconds {
			return nil, media.NewError(media.ErrUnsupported, "edit on %s spans %.0fs, over the 12h single-file limit", entry.Key(), secs)
		}
		seg := Segment{
			Event: timeline.Event{
				Time:   startTick,
				Camera: edit.Camera,
				Track:  edit.Track,
				Seq:    edit.Seq,
				In:     unwrappedIn,
				Out:    unwrappedOut,
				Kind:   "audio",
			},
			Entry:         entry,
			Path:          resolved,
			StartTick:     startTick,
			EndTick:       endTick,
			LengthSamples: length,
			AlignmentUS:   alignUS,
		}
		plan.Segments = append(plan.Segments, seg)
		tl.Add(seg.Event)
	}
	tl.Finish()

	// Re-derive the final order from the timeline (late arrivals may have
	// reordered segments).
	ordered := tl.Events()
	byKey := map[string]int{}
	for i, s := range plan.Segments {
		byKey[eventKey(s.Event)] = i
	}
	sorted := make([]Segment, 0, len(ordered))
	for _, ev := range ordered {
		if i, ok := byKey[eventKey(ev)]; ok {
			sorted = append(sorted, plan.Segments[i])
		}
	}
	if len(sorted) == len(plan.Segments) {
		plan.Segments = sorted
	} else {
		sort.SliceStable(plan.Segments, func(i, j int) bool {
			return timeline.Compare(plan.Segments[i].Event, plan.Segments[j].Event) < 0
		})
	}
	plan.Events = ordered
	plan.Late = tl.LateEvents()
	plan.Replayed = tl.ReplayedEvents()
	return plan, nil
}

// MaxAssetSeconds is the 12 hour ceiling for a single listed asset.
const MaxAssetSeconds = 12 * 60 * 60

// timeSpanSeconds converts a raw tick span to seconds.
func timeSpanSeconds(tb media.Rational, from, to int64) float64 {
	if tb.Num <= 0 || tb.Den <= 0 {
		return 0
	}
	return float64(to-from) * tb.Float64()
}

func eventKey(e timeline.Event) string {
	return e.Camera + "\x00" + e.Track + "\x00" + time.Duration(e.Time).String() + "\x00" + media.FormatInt(e.Seq)
}

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
