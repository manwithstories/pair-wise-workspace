// Package timeline composes tracks onto a single 90 kHz rational timeline. It
// expands 32-bit timestamp wraps, fits node clock drift from synchronisation
// anchors, and refuses to align a node whose drift exceeds 500 ppm.
package timeline

import (
	"fmt"
	"sort"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/model"
)

// MaxDriftPPM is the alignment bound. A node whose estimated drift exceeds it
// is marked unalignable instead of being silently placed.
const MaxDriftPPM = 500

// Anchor is a synchronisation point: a node clock reading at a known local
// sample time. Both times are in 90 kHz ticks.
type Anchor struct {
	Index    int
	NodeTick int64
	Local90k int64
}

// DriftFit is the result of a least-squares fit over the anchors.
type DriftFit struct {
	PPM       int64 // estimated drift in parts per million (node vs local)
	Unaligned bool  // true when |PPM| > MaxDriftPPM or the fit is degenerate
	Anchors   int
	// NodeOffset is the fitted node tick at local tick 0.
	NodeOffset int64
	Residual   int64 // largest absolute residual in ticks
	Reason     string
}

// FitDrift estimates node clock drift by least squares over the anchors. The
// model is node = local*(1+ppm/1e6) + offset, so a positive PPM means the node
// clock runs fast.
func FitDrift(anchors []Anchor) DriftFit {
	n := len(anchors)
	if n < 2 {
		return DriftFit{Unaligned: true, Anchors: n, Reason: "need at least two anchors"}
	}
	// Use the mean-centred form to keep 64-bit math exact for the slope.
	var sumX, sumY int64
	for _, a := range anchors {
		sumX += a.Local90k
		sumY += a.NodeTick
	}
	meanX := sumX / int64(n)
	meanY := sumY / int64(n)
	var num, den int64
	for _, a := range anchors {
		dx := a.Local90k - meanX
		dy := a.NodeTick - meanY
		num += dx * dy
		den += dx * dx
	}
	if den == 0 {
		return DriftFit{Unaligned: true, Anchors: n, Reason: "all anchors share a local timestamp"}
	}
	// slope = num/den, expressed as a rational to avoid float drift.
	slopeNum, slopeDen := num, den
	if slopeDen < 0 {
		slopeNum, slopeDen = -slopeNum, -slopeDen
	}
	// ppm = (slope - 1) * 1e6 = (num - den) * 1e6 / den
	ppm := divRound((slopeNum-slopeDen)*1000000, slopeDen)
	offset := meanY - mulDiv(slopeNum, meanX, slopeDen)
	fit := DriftFit{PPM: ppm, Anchors: n, NodeOffset: offset}
	for _, a := range anchors {
		pred := mulDiv(slopeNum, a.Local90k, slopeDen) + offset
		if d := abs64(pred - a.NodeTick); d > fit.Residual {
			fit.Residual = d
		}
	}
	if abs64(ppm) > MaxDriftPPM {
		fit.Unaligned = true
		fit.Reason = fmt.Sprintf("drift %d ppm exceeds bound %d ppm", ppm, MaxDriftPPM)
	}
	return fit
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func divRound(n, d int64) int64 {
	if d == 0 {
		return 0
	}
	if d < 0 {
		n, d = -n, -d
	}
	q, r := n/d, n%d
	if r < 0 {
		r += d
		q--
	}
	if 2*r >= d {
		return q + 1
	}
	return q
}

// mulDiv computes a*b/c without overflowing for the magnitudes used here.
func mulDiv(a, b, c int64) int64 {
	if c == 0 {
		return 0
	}
	// Split a*b into high and low halves when the product would overflow.
	if a != 0 && abs64(b) > (1<<62)/abs64(a) {
		q := a / c
		r := a % c
		return q*b + r*b/c
	}
	return a * b / c
}

// Entry is one sample placed on the composed timeline.
type Entry struct {
	Track     int
	TrackID   uint32
	SampleIdx int
	Start90k  int64
	Dur90k    int64
	Sync      bool
	Codec     model.Codec
	// Seq is the deterministic order of the sample within the source.
	Seq int
}

// End returns the exclusive end tick of the entry.
func (e Entry) End() int64 { return e.Start90k + e.Dur90k }

// TrackSource is a track plus the clock information needed to place it.
type TrackSource struct {
	Track   *model.Track
	Anchors []Anchor
	// FromNode marks a track whose sample timestamps were taken from the
	// remote node clock (RTP). Such timestamps must be mapped back through
	// the fitted drift before they may be compared with anything local.
	FromNode bool
	// NodeTicks holds one node-clock reading per sample when FromNode is set.
	// It is indexed by sample index and must have the same length.
	NodeTicks []int64
}

// NodeToLocal maps a node-clock tick to the local 90 kHz timeline using the
// inverse of the fitted drift model.
func (f DriftFit) NodeToLocal(nodeTick int64) int64 {
	slopeNum := int64(1000000 + f.PPM)
	if slopeNum == 0 {
		return nodeTick - f.NodeOffset
	}
	return mulDiv(nodeTick-f.NodeOffset, 1000000, slopeNum)
}

// LocalToNode maps a local 90 kHz tick onto the node clock.
func (f DriftFit) LocalToNode(localTick int64) int64 {
	slopeNum := int64(1000000 + f.PPM)
	return mulDiv(slopeNum, localTick, 1000000) + f.NodeOffset
}

// ComposeOptions tunes composition.
type ComposeOptions struct {
	// Base90k is the timeline origin in 90 kHz ticks. Samples starting before
	// it are dropped.
	Base90k int64
	// MaxEntries caps the composed timeline length.
	MaxEntries int
	// RequireMJPEG limits composition to decodable video tracks.
	RequireMJPEG bool
}

// Composition is the composed timeline plus per-track diagnostics.
type Composition struct {
	Entries    []Entry
	Drift      map[int]DriftFit
	Unaligned  []int // track numbers excluded for excessive drift
	Discarded  int   // samples starting before Base90k
	VFRTracks  int   // tracks with at least three distinct sample durations
	MaxIntervals int
	Faults     *fault.List
}

// Compose places every track's samples on one timeline. Samples keep their own
// durations: nothing is padded to a fixed frame rate.
func Compose(sources []TrackSource, opts ComposeOptions) Composition {
	if opts.MaxEntries == 0 {
		opts.MaxEntries = 1 << 20
	}
	comp := Composition{
		Drift:  map[int]DriftFit{},
		Faults: &fault.List{},
	}
	distinct := map[int]map[int64]bool{}
	for _, src := range sources {
		trak := src.Track
		if trak == nil {
			continue
		}
		if opts.RequireMJPEG && trak.Kind == model.Video && !trak.Codec.Decodable() {
			comp.Faults.Add(fault.New(trak.Number, 0, fault.KindUnsupported, true,
				"track %s codec %s is not decodable; excluded from composition", trak.Label(), trak.Codec))
			continue
		}
		anchors := src.Anchors
		fit := DriftFit{Anchors: len(anchors)}
		if len(anchors) >= 2 {
			fit = FitDrift(anchors)
			comp.Drift[trak.Number] = fit
			if fit.Unaligned {
				comp.Unaligned = append(comp.Unaligned, trak.Number)
				comp.Faults.Add(fault.New(trak.Number, 0, fault.KindDrift, true,
					"track %s marked unalignable: %s", trak.Label(), fit.Reason))
				continue
			}
		} else if len(anchors) == 1 {
			fit.NodeOffset = anchors[0].NodeTick - anchors[0].Local90k
		}
		// Drift correction retimes node-sourced samples through the inverse
		// fit. Durations are never rewritten, so VFR survives correction.
		for _, s := range trak.Samples {
			dts, err := trak.DTS90k(s)
			if err != nil {
				comp.Faults.Add(fault.Wrap(trak.Number, s.Offset, fault.KindCorrupt, false, err))
				continue
			}
			dur := sampleDur90k(trak, s)
			start := dts
			if src.FromNode {
				if s.Index < 0 || s.Index >= len(src.NodeTicks) {
					comp.Faults.Add(fault.New(trak.Number, 0, fault.KindCorrupt, false,
						"track %s node ticks missing for sample %d", trak.Label(), s.Index))
					continue
				}
				nodeTick := src.NodeTicks[s.Index]
				if len(anchors) >= 2 {
					start = fit.NodeToLocal(nodeTick)
				} else {
					start = nodeTick - fit.NodeOffset
				}
			}
			if start < opts.Base90k {
				comp.Discarded++
				continue
			}
			if len(comp.Entries) >= opts.MaxEntries {
				comp.Faults.Add(fault.New(trak.Number, 0, fault.KindLimit, false,
					"composed timeline exceeds %d entries", opts.MaxEntries))
				return comp
			}
			comp.Entries = append(comp.Entries, Entry{
				Track: trak.Number, TrackID: trak.ID, SampleIdx: s.Index,
				Start90k: start, Dur90k: dur, Sync: s.Sync, Codec: s.Codec, Seq: s.Index,
			})
			key := distinct[trak.Number]
			if key == nil {
				key = map[int64]bool{}
				distinct[trak.Number] = key
			}
			key[dur] = true
		}
	}
	for _, durs := range distinct {
		if len(durs) >= 3 {
			comp.VFRTracks++
		}
		if len(durs) > comp.MaxIntervals {
			comp.MaxIntervals = len(durs)
		}
	}
	SortEntries(comp.Entries)
	return comp
}

func sampleDur90k(trak *model.Track, s model.Sample) int64 {
	if trak.Timebase == (model.Timebase{}) {
		return s.Duration
	}
	d, err := trak.Timebase.ConvertTo(s.Duration, model.RTP90k)
	if err != nil || d <= 0 {
		return 1
	}
	return d
}

// SortEntries orders by start time, then track number, then sample index. This
// is the same key the merge stage uses, so both are stable under concurrency.
func SortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Start90k != b.Start90k {
			return a.Start90k < b.Start90k
		}
		if a.Track != b.Track {
			return a.Track < b.Track
		}
		if a.SampleIdx != b.SampleIdx {
			return a.SampleIdx < b.SampleIdx
		}
		return a.Seq < b.Seq
	})
}

// Span returns the first start and the last end tick of the composition.
func (c Composition) Span() (first, last int64) {
	if len(c.Entries) == 0 {
		return 0, 0
	}
	first = c.Entries[0].Start90k
	last = c.Entries[0].End()
	for _, e := range c.Entries {
		if e.Start90k < first {
			first = e.Start90k
		}
		if e.End() > last {
			last = e.End()
		}
	}
	return first, last
}

// Duration90k returns the timeline length in 90 kHz ticks.
func (c Composition) Duration90k() int64 {
	f, l := c.Span()
	return l - f
}
