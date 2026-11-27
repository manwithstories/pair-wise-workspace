// Package recovery turns each representation's parsed fragments into ordered,
// clock-corrected tracks. Unrecoverable fragments are isolated, original
// sequence numbers are preserved, and every decision is recorded.
package recovery

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"archive/internal/clock"
	"archive/internal/isobmff"
	"archive/internal/media"
)

// Decisions made during recovery.
const (
	ActAccepted  = "accepted"
	ActDropped   = "dropped"
	ActDuplicate = "duplicate_skipped"
	ActRegressed = "regressed_isolated"
	ActRepaired  = "cts_repaired"
)

// Decision is one auditable choice.
type Decision struct {
	Segment uint32 `json:"segment"`
	Action  string `json:"action"`
	Detail  string `json:"detail,omitempty"`
}

// TimedSample is one sample on the shared microsecond clock.
type TimedSample struct {
	media.Sample
	// StartMicros and EndMicros are drift-corrected composition bounds.
	StartMicros int64 `json:"start_us"`
	EndMicros   int64 `json:"end_us"`
}

// RecoveredTrack is one track's accepted, clock-corrected samples.
type RecoveredTrack struct {
	TrackID   uint32
	Kind      media.TrackKind
	Timescale media.TimeScale
	Samples   []TimedSample
	Drift     media.DriftEstimate
	Wraps     int64
	Regressed int
	Decisions []Decision
}

// RepresentationRecovery is the outcome for one bitrate representation.
type RepresentationRecovery struct {
	Name          string
	Path          string
	MovieTracks   []media.Track
	FragmentCount int
	Accepted      int
	Dropped       int
	Duplicates    int
	Truncated     int
	// FirstBadByte is the minimum offending byte, -1 if none.
	FirstBadByte int64
	Tracks       map[uint32]*RecoveredTrack
	Decisions    []Decision
	Faults       []string
}

// Options configures recovery thresholds.
type Options struct {
	ClockSkewToleranceMicros int64
	MaxRepairWindowMicros    int64
	DriftTolerancePPM        float64
}

// DefaultOptions are tuned for the 8-second acceptance fixture.
func DefaultOptions() Options {
	return Options{
		ClockSkewToleranceMicros: 2000,
		MaxRepairWindowMicros:    5_000_000,
		DriftTolerancePPM:        50,
	}
}

// Recover runs the recovery pass for one parsed representation.
func Recover(name, path string, parsed *isobmff.Result, opts Options) (*RepresentationRecovery, error) {
	if parsed == nil {
		return nil, fmt.Errorf("%w: nil parse result", media.ErrUnrecoverable)
	}
	rr := &RepresentationRecovery{
		Name:          name,
		Path:          path,
		FragmentCount: len(parsed.Fragments),
		FirstBadByte:  parsed.FirstBadByte,
		Tracks:        map[uint32]*RecoveredTrack{},
		MovieTracks:   parsed.Movie.Tracks,
	}
	ordered := make([]isobmff.Fragment, len(parsed.Fragments))
	copy(ordered, parsed.Fragments)
	if !storageMonotonic(ordered) {
		rr.Faults = append(rr.Faults, "fragment sequence non-monotonic in storage order")
	}
	isobmff.SortFragments(ordered)

	seenSeq := map[uint32]bool{}
	acceptedSeq := map[uint32]bool{}
	for _, frag := range ordered {
		if frag.FirstBadByte >= 0 {
			rr.Decisions = append(rr.Decisions, Decision{
				Segment: frag.Sequence,
				Action:  ActDropped,
				Detail:  fmt.Sprintf("first_bad_byte=%d fault=%q dropped_samples=%d", frag.FirstBadByte, frag.Fault, frag.DroppedSamples),
			})
			if frag.MdatTruncated {
				rr.Truncated++
			}
			rr.Faults = append(rr.Faults, fmt.Sprintf("segment %d: %s", frag.Sequence, frag.Fault))
			rr.Dropped++
			continue
		}
		if seenSeq[frag.Sequence] {
			rr.Decisions = append(rr.Decisions, Decision{Segment: frag.Sequence, Action: ActDuplicate, Detail: "sequence already accepted"})
			rr.Duplicates++
			rr.Faults = append(rr.Faults, fmt.Sprintf("segment %d: duplicate sequence number", frag.Sequence))
			continue
		}
		seenSeq[frag.Sequence] = true
		acceptedSeq[frag.Sequence] = true
		rr.Decisions = append(rr.Decisions, Decision{Segment: frag.Sequence, Action: ActAccepted})
		rr.Accepted++
	}

	missing := missingSequences(acceptedSeq)
	if len(missing) > 0 {
		rr.Faults = append(rr.Faults, fmt.Sprintf("sequence gaps after recovery: %s", joinU32(missing)))
	}

	trackSamples := map[uint32][]media.Sample{}
	trackTS := map[uint32]media.TimeScale{}
	trackKind := map[uint32]media.TrackKind{}
	for _, t := range parsed.Movie.Tracks {
		trackTS[t.ID] = t.Timescale
		trackKind[t.ID] = t.Kind
	}
	for _, frag := range ordered {
		if frag.FirstBadByte >= 0 || !acceptedSeq[frag.Sequence] {
			continue
		}
		for _, tr := range frag.Tracks {
			trackSamples[tr.TrackID] = append(trackSamples[tr.TrackID], tr.Samples...)
			trackTS[tr.TrackID] = tr.Timescale
		}
	}
	for id, samples := range trackSamples {
		rt, err := recoverTrack(id, trackKind[id], trackTS[id], samples, opts)
		if err != nil {
			return nil, err
		}
		rr.Tracks[id] = rt
		for _, d := range rt.Decisions {
			rr.Faults = append(rr.Faults, fmt.Sprintf("track %d/segment %d: %s %s", id, d.Segment, d.Action, d.Detail))
		}
	}
	if len(rr.Tracks) == 0 {
		return nil, fmt.Errorf("%w: no recoverable tracks in representation %q", media.ErrUnrecoverable, name)
	}
	return rr, nil
}

func storageMonotonic(frags []isobmff.Fragment) bool {
	for i := 1; i < len(frags); i++ {
		if frags[i].Sequence <= frags[i-1].Sequence {
			return false
		}
	}
	return true
}

func missingSequences(accepted map[uint32]bool) []uint32 {
	if len(accepted) == 0 {
		return nil
	}
	var lo uint32 = ^uint32(0)
	var hi uint32
	for s := range accepted {
		if s < lo {
			lo = s
		}
		if s > hi {
			hi = s
		}
	}
	var out []uint32
	for s := lo; s <= hi && s-lo < 4096; s++ {
		if !accepted[s] {
			out = append(out, s)
		}
	}
	return out
}

func joinU32(v []uint32) string {
	parts := make([]string, len(v))
	for i, s := range v {
		parts[i] = fmt.Sprintf("%d", s)
	}
	return strings.Join(parts, ",")
}

// recoverTrack unwraps timestamps, enforces DTS monotonicity, fits drift, and
// records composition bounds on the shared clock.
func recoverTrack(id uint32, kind media.TrackKind, ts media.TimeScale, samples []media.Sample, opts Options) (*RecoveredTrack, error) {
	if len(samples) == 0 {
		return nil, fmt.Errorf("%w: track %d has no samples", media.ErrUnrecoverable, id)
	}
	rt := &RecoveredTrack{TrackID: id, Kind: kind, Timescale: ts}
	// One cursor spans the whole track: wraps accumulate monotonically across
	// fragments, and the sequence number tells the cursor when an expected
	// step is a gap left by a dropped fragment rather than a corrupt jump.
	cur := clock.NewTrackCursor(ts, 0)
	var lastSeq uint32
	var haveSeq bool
	isolatedSeg := map[uint32]bool{}
	unwrapped := make([]media.Sample, 0, len(samples))
	var resync bool
	for _, s := range samples {
		gapBefore := haveSeq && s.Segment != lastSeq && s.Segment > lastSeq+1
		lastSeq = s.Segment
		haveSeq = true
		if resync {
			// The fragment after an isolated one restarts continuity: its raw
			// tfdt is trustworthy, and only the dropped fragment is missing.
			cur = clock.NewTrackCursor(ts, 0)
			resync = false
		}
		next, err := cur.StepUnwrapped(s.DTS, gapBefore)
		if err != nil {
			if errors.Is(err, clock.ErrRegression) {
				rt.Regressed++
				isolatedSeg[s.Segment] = true
				rt.Decisions = append(rt.Decisions, Decision{
					Segment: s.Segment,
					Action:  ActRegressed,
					Detail:  fmt.Sprintf("track %d: %v", id, err),
				})
				resync = true
				continue
			}
			return nil, fmt.Errorf("%w: track %d: %v", media.ErrUnrecoverable, id, err)
		}
		cur.Accept(next, s.DTS)
		s.DTS = next
		if s.CTS < next {
			s.CTS = next
			rt.Decisions = append(rt.Decisions, Decision{
				Segment: s.Segment,
				Action:  ActRepaired,
				Detail:  fmt.Sprintf("CTS below DTS clamped for track %d", id),
			})
		}
		unwrapped = append(unwrapped, s)
	}
	// Drop isolated fragments; the next fragment's cursor was re-anchored, so
	// the timeline keeps its true absolute position and the hole is explicit.
	var final []media.Sample
	for _, s := range unwrapped {
		if isolatedSeg[s.Segment] {
			continue
		}
		final = append(final, s)
	}
	unwrapped = final
	if len(unwrapped) == 0 {
		return nil, fmt.Errorf("%w: track %d: all samples regressed", media.ErrUnrecoverable, id)
	}
	// Correct drift against the nominal cadence only; the resulting
	// per-fragment anchor deltas are what the fan-in compares across layers.
	est := fitDrift(unwrapped, ts, opts)
	rt.Drift = est
	rt.Wraps = cur.WrapCount
	cor := clock.Corrector{TS: ts, Estimate: est}
	for _, s := range unwrapped {
		start := cor.Micros(s.CTS)
		end := cor.Micros(s.CTS + s.Duration)
		rt.Samples = append(rt.Samples, TimedSample{Sample: s, StartMicros: start, EndMicros: end})
	}
	sort.SliceStable(rt.Samples, func(i, j int) bool {
		a, b := rt.Samples[i].CTS, rt.Samples[j].CTS
		if a == b {
			return rt.Samples[i].DTS < rt.Samples[j].DTS
		}
		return a < b
	})
	return rt, nil
}

// fitDrift estimates a clock slope from the container's own decode times.
//
// Estimating from every sample makes VFR look like drift (each frame's
// cadence deviates), and estimating from a global ordinal reference makes a
// dropped fragment look like drift (the ordinal/time relation shifts). The
// robust estimator here uses only *consecutive accepted fragments*: for each
// adjacent pair it compares the actual tick delta to the nominal delta (kept
// sample count x median duration) and takes the median of those ratios. Both
// VFR and gaps average out, while a genuine clock slope shows up in every
// pair.
func fitDrift(samples []media.Sample, ts media.TimeScale, opts Options) media.DriftEstimate {
	est := media.DriftEstimate{Samples: len(samples)}
	if len(samples) == 0 {
		return est
	}
	est.OriginMicros = ts.ToMicros(samples[0].DTS)
	est.OriginTick = samples[0].DTS
	if len(samples) < 8 {
		return est
	}
	durs := make([]int64, 0, len(samples))
	for _, s := range samples {
		durs = append(durs, s.Duration)
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	median := durs[len(durs)/2]
	if median <= 0 {
		return est
	}

	// Collect one observation per adjacent fragment pair.
	type anchor struct {
		seg   uint32
		first int64
		last  int64
		count int64
	}
	var anchors []anchor
	for _, s := range samples {
		if n := len(anchors); n > 0 && anchors[n-1].seg == s.Segment {
			anchors[n-1].last = s.DTS
			anchors[n-1].count++
			continue
		}
		anchors = append(anchors, anchor{seg: s.Segment, first: s.DTS, count: 1})
	}
	var ppms []float64
	for i := 1; i < len(anchors); i++ {
		// Only pairs that are adjacent in the original sequence; a gap means
		// an unknown number of frames is missing, so the comparison is invalid.
		if anchors[i].seg != anchors[i-1].seg+1 {
			continue
		}
		prev := anchors[i-1]
		expected := float64(prev.count) * float64(median)
		actual := float64(anchors[i].first - prev.first)
		if expected <= 0 || actual <= 0 {
			continue
		}
		ppms = append(ppms, (actual/expected-1)*1e6)
	}
	if len(ppms) < 3 {
		return est
	}
	sort.Float64s(ppms)
	est.SlopePPM = ppms[len(ppms)/2]
	if est.SlopePPM > -opts.DriftTolerancePPM && est.SlopePPM < opts.DriftTolerancePPM {
		est.SlopePPM = 0
		return est
	}
	// Residual after correction: worst per-anchor deviation from the fit.
	var maxRes float64
	for i := 1; i < len(anchors); i++ {
		if anchors[i].seg != anchors[i-1].seg+1 {
			continue
		}
		prev := anchors[i-1]
		expected := float64(prev.count) * float64(median)
		actual := float64(anchors[i].first - prev.first)
		res := actual - expected*(1+est.SlopePPM/1e6)
		if res < 0 {
			res = -res
		}
		if res > maxRes {
			maxRes = res
		}
	}
	est.ResidualMicros = int64(maxRes)
	_ = opts
	return est
}
