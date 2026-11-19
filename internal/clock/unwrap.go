// Package clock turns per-track source clocks into the unified integer-nanosecond
// program timeline. It unwraps counter rollover, repairs repeated and backward
// timestamps without dropping samples, and estimates drift from trusted anchors
// with piecewise fits that are never extrapolated across a detected breakpoint.
package clock

import (
	"fmt"
	"math"
	"sort"

	"archive.local/recover/internal/errs"
	"archive.local/recover/internal/model"
)

// Defaults for drift handling. Callers may tighten them; zero means default.
const (
	DefaultWrapBits    = 32
	DefaultBreakNs     = 20_000_000 // 20 ms residual jump marks a new segment
	DefaultMaxDriftPPM = 50_000     // accept up to 5% clock rate deviation
	// DefaultMaxExtrapNs bounds how far beyond the segment's own anchors a fit
	// may predict. It is deliberately larger than a typical 10 s anchor gap so
	// ordinary drift correction is unaffected, but a capture with only a couple
	// of anchors cannot silently extrapolate across minutes.
	DefaultMaxExtrapNs = 60_000_000_000
	MaxSamples         = 5_000_000
)

// Anchor is a trusted program-time observation for a sample index.
type Anchor struct {
	Sample    int    `json:"sample"`
	ProgramNs int64  `json:"program_ns"`
	Source    string `json:"source,omitempty"`
}

// Input describes one track's raw clock observations.
type Input struct {
	TrackID     uint32
	Kind        string
	Timescale   uint32
	RawPTS      []int64
	Durations   []int64 // optional, in timescale ticks
	Keyframes   []int
	Anchors     []Anchor
	WrapBits    uint
	BreakNs     int64
	MaxDriftPPM int64
	MaxExtrapNs int64
	File        string
}

// Result is the recovered mapping plus the audit trail.
type Result struct {
	Track       *model.TrackTimeline
	Issues      errs.List
	Wraps       int
	Repeats     int
	Backward    int
	Segments    [][2]int
	Breakpoints []int
	AnchorCount int
}

// Unwrap maps one track's clock onto program time.
func Unwrap(in Input) (*Result, error) {
	if len(in.RawPTS) == 0 {
		return nil, fmt.Errorf("track %d: no timestamps", in.TrackID)
	}
	if len(in.RawPTS) > MaxSamples {
		return nil, fmt.Errorf("track %d: %d samples exceeds limit %d", in.TrackID, len(in.RawPTS), MaxSamples)
	}
	bits := in.WrapBits
	if bits == 0 {
		bits = DefaultWrapBits
	}
	if bits < 8 || bits > 62 {
		return nil, fmt.Errorf("track %d: wrap bits %d out of range", in.TrackID, bits)
	}
	r := &Result{}
	loc := func(sample int) errs.Location {
		return errs.Location{File: in.File, TrackID: in.TrackID, Kind: in.Kind, Sample: sample, Offset: -1}
	}

	// 1. Unwrap the counter. A rollover is only credible when the previous
	//    value was near the top of the counter range and the new value is near
	//    the bottom; a large backward jump between arbitrary values is a real
	//    discontinuity and must stay visible to the repair and drift stages
	//    rather than being re-based by a full range.
	wrap := int64(1) << bits
	nearTop := wrap - wrap/16
	nearBottom := wrap / 16
	unwrapped := make([]int64, len(in.RawPTS))
	var offset int64
	unwrapped[0] = in.RawPTS[0]
	for i := 1; i < len(in.RawPTS); i++ {
		v := in.RawPTS[i] + offset
		prev := unwrapped[i-1]
		prevRaw := in.RawPTS[i-1]
		curRaw := in.RawPTS[i]
		if v < prev && prev-v > wrap/2 && prevRaw >= nearTop && curRaw <= nearBottom {
			offset += wrap
			v = in.RawPTS[i] + offset
			r.Wraps++
			r.Issues.Add(errs.ClassClock, errs.CodeClockWrap,
				fmt.Sprintf("counter wrapped at sample %d: %d -> %d", i, in.RawPTS[i-1], in.RawPTS[i]), loc(i))
		}
		unwrapped[i] = v
	}

	// 2. Convert to nominal nanoseconds and repair non-monotonic samples. Every
	//    input sample yields exactly one timeline entry.
	nominal, repeats, backward := repair(in.RawPTS, unwrapped, in.Timescale, in.TrackID, in.Kind, in.File, &r.Issues, loc)
	r.Repeats, r.Backward = repeats, backward

	// 3. Fit a piecewise drift affine against anchors.
	segments, models, breaks, issues, fatal := fitDrift(nominal, in)
	r.Segments, r.Breakpoints = segments, breaks
	r.AnchorCount = len(in.Anchors)
	r.Issues = append(r.Issues, issues...)
	if fatal {
		// A fit outside the accepted drift band means the mapping is not
		// credible; refuse to emit a timeline rather than apply it.
		return r, fmt.Errorf("track %d: fitted drift exceeds the accepted band", in.TrackID)
	}

	// 4. Apply the model. Each segment only ever maps its own sample range, so
	//    no prediction crosses a breakpoint. Extrapolation beyond MaxExtrapNs
	//    from the segment's anchors is clamped so a sparse fit cannot silently
	//    run away, and the clamp is reported once per track.
	pts := make([]int64, len(nominal))
	segIdx := 0
	clamped := false
	for i := range nominal {
		for segIdx+1 < len(segments) && i >= segments[segIdx+1][0] {
			segIdx++
		}
		m := models[segIdx]
		if m.clamps(nominal[i]) {
			clamped = true
		}
		pts[i] = m.apply(nominal[i])
		if i > 0 && pts[i] <= pts[i-1] {
			// Float rounding in a fitted segment must never reverse an ordered
			// clock. Pin the sample one tick after its predecessor; no sample is
			// dropped and the input-level counters stay accurate.
			pts[i] = pts[i-1] + 1
		}
	}
	if clamped {
		r.Issues.Add(errs.ClassClock, errs.CodeClockExtrapolation,
			fmt.Sprintf("drift fit extrapolated beyond %.0f ms from its anchors; limited to unit slope", mExtrapMs(in)),
			loc(0))
	}

	tr := &model.TrackTimeline{
		TrackID:   in.TrackID,
		Kind:      in.Kind,
		Timescale: in.Timescale,
		PTS:       pts,
		RawPTS:    append([]int64(nil), in.RawPTS...),
		Keyframes: append([]int(nil), in.Keyframes...),
		Reverse:   r.Backward,
		Wrap:      r.Wraps,
		Anchors:   len(in.Anchors),
		Segments:  append([][2]int(nil), segments...),
	}
	if len(in.Durations) == len(in.RawPTS) {
		tr.Duration = make([]int64, len(in.Durations))
		for i, d := range in.Durations {
			tr.Duration[i] = model.ClockToNs(d, in.Timescale)
		}
	}
	if len(models) > 0 {
		tr.DriftPPM = models[0].ppm
	}
	r.Track = tr
	return r, nil
}

// repair converts unwrapped ticks to nominal ns and enforces non-decreasing
// order without dropping any sample.
func repair(raw, unwrapped []int64, timescale uint32, trackID uint32, kind, file string, issues *errs.List, loc func(int) errs.Location) ([]int64, int, int) {
	nominal := make([]int64, len(unwrapped))
	for i, v := range unwrapped {
		nominal[i] = model.ClockToNs(v, timescale)
	}
	step := typicalStep(nominal)
	// Classify against the original (unrepaired) timeline first, so a repair of
	// one sample cannot fabricate anomalies in the samples that follow it.
	orig := append([]int64(nil), nominal...)
	var repeats, backward int
	for i := 1; i < len(orig); i++ {
		if orig[i] > orig[i-1] {
			continue
		}
		if orig[i] == orig[i-1] {
			repeats++
			issues.Add(errs.ClassClock, errs.CodeClockRepeat,
				fmt.Sprintf("duplicate timestamp at sample %d (raw %d)", i, raw[i]), loc(i))
		} else {
			backward++
			issues.Add(errs.ClassClock, errs.CodeClockBackward,
				fmt.Sprintf("timestamp regressed at sample %d: %d < %d", i, orig[i], orig[i-1]), loc(i))
		}
	}
	// Then enforce non-decreasing program time without dropping anything.
	for i := 1; i < len(nominal); i++ {
		if nominal[i] <= nominal[i-1] {
			nominal[i] = nominal[i-1] + step
		}
	}
	return nominal, repeats, backward
}

// typicalStep returns the median positive inter-sample duration in ns, clamped
// to at least 1 ns. This is the spacing given to repaired samples so that VFR
// content keeps its original cadence as far as possible.
func typicalStep(nominal []int64) int64 {
	deltas := make([]int64, 0, len(nominal))
	for i := 1; i < len(nominal); i++ {
		if d := nominal[i] - nominal[i-1]; d > 0 {
			deltas = append(deltas, d)
		}
	}
	if len(deltas) == 0 {
		return 1
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
	step := deltas[len(deltas)/2]
	if step < 1 {
		step = 1
	}
	return step
}

// affine is a piecewise affine map: program = m*nominal + c.
type affine struct {
	m   float64
	c   int64
	ppm int64
	// loN/hiN delimit the nominal range covered by the segment's anchors, with
	// trusted program times loP/hiP at those endpoints. Outside that span the
	// map may only be extrapolated by maxExtrap.
	loN, hiN, loP, hiP int64
	maxExtrap          int64
}

// apply maps a nominal time. Between the segment's anchors the fitted slope is
// used; beyond them by more than maxExtrap the map falls back to unit slope
// from the nearest anchor, bounding the error a sparse fit can introduce while
// still honouring drift inside the observed span.
func (m affine) apply(nominal int64) int64 {
	if m.maxExtrap > 0 {
		if nominal < m.loN-m.maxExtrap {
			return m.loP + (nominal - m.loN)
		}
		if nominal > m.hiN+m.maxExtrap {
			return m.hiP + (nominal - m.hiN)
		}
	}
	return m.c + int64(float64(nominal)*m.m)
}

// clamps reports whether apply would leave the fitted span for nominal.
func (m affine) clamps(nominal int64) bool {
	if m.maxExtrap <= 0 {
		return false
	}
	return nominal < m.loN-m.maxExtrap || nominal > m.hiN+m.maxExtrap
}

// mExtrapMs renders the configured extrapolation limit for messages.
func mExtrapMs(in Input) float64 {
	v := in.MaxExtrapNs
	if v <= 0 {
		v = DefaultMaxExtrapNs
	}
	return float64(v) / 1e6
}

// fitDrift partitions samples at anchor residual jumps and fits one affine per
// segment. Without anchors the identity map is used and nothing is predicted.
func fitDrift(nominal []int64, in Input) ([][2]int, []affine, []int, errs.List, bool) {
	var issues errs.List
	loc := func(sample int) errs.Location {
		return errs.Location{File: in.File, TrackID: in.TrackID, Kind: in.Kind, Sample: sample, Offset: -1}
	}
	n := len(nominal)
	identity := affine{m: 1}
	if len(in.Anchors) == 0 {
		return [][2]int{{0, n}}, []affine{identity}, nil, issues, false
	}
	anchors := make([]Anchor, 0, len(in.Anchors))
	seen := map[int]bool{}
	for _, a := range in.Anchors {
		if a.Sample < 0 || a.Sample >= n {
			issues.Add(errs.ClassClock, errs.CodeClockAnchorBad,
				fmt.Sprintf("anchor sample %d out of range [0,%d)", a.Sample, n), loc(a.Sample))
			continue
		}
		if seen[a.Sample] {
			continue
		}
		seen[a.Sample] = true
		anchors = append(anchors, a)
	}
	if len(anchors) == 0 {
		return [][2]int{{0, n}}, []affine{identity}, nil, issues, false
	}
	sort.Slice(anchors, func(i, j int) bool { return anchors[i].Sample < anchors[j].Sample })

	breakNs := in.BreakNs
	if breakNs <= 0 {
		breakNs = DefaultBreakNs
	}
	// Group anchors into segments whose residual offsets agree. A residual jump
	// beyond breakNs starts a new segment. The breakpoint is placed at the
	// actual source-clock discontinuity between the two anchors, not at the
	// following anchor, so the first segment is never extrapolated past it.
	type seg struct {
		first, last int // anchor index range [first,last]
		breakAt     int // sample where this segment starts (0 for the first)
	}
	segs := []seg{{first: 0, last: 0}}
	for i := 1; i < len(anchors); i++ {
		d := (anchors[i].ProgramNs - nominal[anchors[i].Sample]) -
			(anchors[i-1].ProgramNs - nominal[anchors[i-1].Sample])
		if d > breakNs || d < -breakNs {
			at := locateBreak(nominal, anchors[i-1], anchors[i])
			segs = append(segs, seg{first: i, last: i, breakAt: at})
			issues.Add(errs.ClassClock, errs.CodeClockDriftBreak,
				fmt.Sprintf("drift break at sample %d between anchors %d and %d (residual jump %.3f ms)",
					at, anchors[i-1].Sample, anchors[i].Sample, float64(d)/1e6), loc(at))
			continue
		}
		segs[len(segs)-1].last = i
	}

	segments := make([][2]int, 0, len(segs))
	models := make([]affine, 0, len(segs))
	breaks := make([]int, 0, len(segs)-1)
	maxPPM := in.MaxDriftPPM
	if maxPPM <= 0 {
		maxPPM = DefaultMaxDriftPPM
	}
	maxExtrap := in.MaxExtrapNs
	if maxExtrap <= 0 {
		maxExtrap = DefaultMaxExtrapNs
	}
	fatal := false
	for k, s := range segs {
		startSample := 0
		if k > 0 {
			startSample = s.breakAt
			breaks = append(breaks, startSample)
		}
		endSample := n
		if k+1 < len(segs) {
			endSample = segs[k+1].breakAt
		}
		if startSample >= endSample {
			endSample = startSample + 1
		}
		m := fitLine(nominal, anchors[s.first:s.last+1])
		m.maxExtrap = maxExtrap
		// The extrapolation bound is measured from the span the segment's
		// anchors actually cover; its endpoints carry trusted program times.
		m.loN = nominal[anchors[s.first].Sample]
		m.hiN = nominal[anchors[s.last].Sample]
		m.loP = anchors[s.first].ProgramNs
		m.hiP = anchors[s.last].ProgramNs
		if m.ppm > maxPPM || m.ppm < -maxPPM {
			fatal = true
			issues.AddFatal(errs.ClassClock, errs.CodeClockDriftBreak,
				fmt.Sprintf("fitted drift %d ppm exceeds limit %d ppm in segment %d", m.ppm, maxPPM, k),
				loc(anchors[s.first].Sample))
		}
		segments = append(segments, [2]int{startSample, endSample})
		models = append(models, m)
	}
	return segments, models, breaks, issues, fatal
}

// locateBreak finds the sample where the source clock jumped: the inter-sample
// step in (prev.Sample, next.Sample] that deviates most from the local cadence
// is the discontinuity. When no step stands out (the jump was spread across a
// gap wider than the anchors), the newer anchor is the safe boundary, since
// every sample before it is still covered by the previous fit.
func locateBreak(nominal []int64, prev, next Anchor) int {
	lo, hi := prev.Sample, next.Sample
	if hi-lo < 2 {
		return hi
	}
	// Local cadence from the samples between the anchors.
	var sum int64
	var cnt int64
	for i := lo + 1; i <= hi; i++ {
		if d := nominal[i] - nominal[i-1]; d > 0 {
			sum += d
			cnt++
		}
	}
	if cnt == 0 {
		return hi
	}
	cadence := sum / cnt
	bestAt := hi
	var bestDev int64
	for i := lo + 1; i <= hi; i++ {
		dev := nominal[i] - nominal[i-1] - cadence
		if dev < 0 {
			dev = -dev
		}
		if dev > bestDev {
			bestDev, bestAt = dev, i
		}
	}
	// Require the outlier to be a real discontinuity rather than ordinary VFR
	// jitter: at least a quarter of the observed residual jump.
	jump := (next.ProgramNs - nominal[next.Sample]) - (prev.ProgramNs - nominal[prev.Sample])
	if jump < 0 {
		jump = -jump
	}
	if bestDev*4 < jump {
		return hi
	}
	return bestAt
}

// fitLine computes a least-squares affine fit of anchor program times against
// nominal times, working in offsets from the first anchor so float error stays
// bounded for day-long captures.
func fitLine(nominal []int64, anchors []Anchor) affine {
	if len(anchors) == 1 {
		a := anchors[0]
		return affine{m: 1, c: a.ProgramNs - nominal[a.Sample], ppm: 0}
	}
	baseN := nominal[anchors[0].Sample]
	baseP := anchors[0].ProgramNs
	var sx, sy, sxx, sxy float64
	nn := float64(len(anchors))
	for _, a := range anchors {
		x := float64(nominal[a.Sample] - baseN)
		y := float64(a.ProgramNs - baseP)
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	den := nn*sxx - sx*sx
	m := 1.0
	if den != 0 {
		m = (nn*sxy - sx*sy) / den
	}
	// Reject non-finite or absurd slopes before they reach the int64
	// conversion: such a fit is corrupt, and letting it through would both
	// overflow the map and hide the error behind a wrapped ppm value.
	if math.IsNaN(m) || math.IsInf(m, 0) {
		m = 1
	}
	if m > 4 {
		m = 4
	} else if m < 0.25 {
		m = 0.25
	}
	intercept := int64(float64(baseP) - m*float64(baseN))
	ppm := (m - 1) * 1e6
	if ppm > math.MaxInt64/2 {
		ppm = math.MaxInt64 / 2
	} else if ppm < math.MinInt64/2 {
		ppm = math.MinInt64 / 2
	}
	return affine{m: m, c: intercept, ppm: int64(ppm)}
}
