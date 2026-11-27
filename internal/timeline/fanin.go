// Package timeline fans multiple bitrate representations into one shared
// clock. It does not silently pick a layer: any boundary disagreement beyond
// the configured threshold is surfaced as a conflict.
package timeline

import (
	"fmt"
	"sort"

	"archive/internal/media"
	"archive/internal/recovery"
)

// Options configures fan-in.
type Options struct {
	// BoundaryThresholdUS is the largest allowed disagreement between
	// representations on a shared track's start/end/sample-count-derived span.
	BoundaryThresholdUS int64
	// SharedOriginUS is the clock zero every representation is aligned to.
	SharedOriginUS int64
}

// DefaultOptions is the acceptance contract: 2000 us boundary delta.
func DefaultOptions() Options {
	return Options{BoundaryThresholdUS: 2000, SharedOriginUS: 0}
}

// Input is one representation's recovery result.
type Input struct {
	Name string
	Rec *recovery.RepresentationRecovery
}

// Build assembles the shared-clock timeline from representations. Tracks are
// grouped by (kind, ordinal) so that representations with the same ladder
// shape line up. The highest-quality representation (first input) is used as
// the sample carrier per track, but a boundary conflict is never resolved by
// switching layers silently: it is recorded.
func Build(inputs []Input, opts Options) (*media.Timeline, error) {
	if len(inputs) == 0 {
		return nil, fmt.Errorf("%w: no representations to fan in", media.ErrUnrecoverable)
	}
	tl := &media.Timeline{OriginUS: opts.SharedOriginUS}
	// Group each representation's tracks into (kind, ordinal) buckets.
	groupOf := func(rec *recovery.RepresentationRecovery) map[string]*recovery.RecoveredTrack {
		out := map[string]*recovery.RecoveredTrack{}
		video, audio := 0, 0
		ids := make([]uint32, 0, len(rec.Tracks))
		for id := range rec.Tracks {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			rt := rec.Tracks[id]
			switch rt.Kind {
			case media.KindVideo:
				out[fmt.Sprintf("%s/%d", rt.Kind, video)] = rt
				video++
			case media.KindAudio:
				out[fmt.Sprintf("%s/%d", rt.Kind, audio)] = rt
				audio++
			}
		}
		return out
	}

	type slot struct {
		kind       media.TrackKind
		ordinal    int
		repName    string
		trackID    uint32
		ts         media.TimeScale
		samples    []media.TimelineSample
		boundaries []media.Boundary
	}
	slots := map[string]*slot{}
	order := []string{}
	for _, in := range inputs {
		if in.Rec == nil {
			return nil, fmt.Errorf("%w: representation %q has no recovery", media.ErrUnrecoverable, in.Name)
		}
		tl.Representations = append(tl.Representations, in.Name)
		groups := groupOf(in.Rec)
		keys := make([]string, 0, len(groups))
		for k := range groups {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			rt := groups[key]
			if len(rt.Samples) == 0 {
				return nil, fmt.Errorf("%w: representation %q track %q empty", media.ErrUnrecoverable, in.Name, key)
			}
			sp, ok := slots[key]
			if !ok {
				sp = &slot{kind: rt.Kind, repName: in.Name, trackID: rt.TrackID, ts: rt.Timescale}
				slots[key] = sp
				order = append(order, key)
			}
			b := media.Boundary{
				Representation: in.Name,
				TrackID:        rt.TrackID,
				Kind:           rt.Kind,
				StartUS:        rt.Samples[0].StartMicros,
				EndUS:          rt.Samples[len(rt.Samples)-1].EndMicros,
				SampleCount:    len(rt.Samples),
			}
			sp.boundaries = append(sp.boundaries, b)
			tl.Boundaries = append(tl.Boundaries, b)
			// Only the first (highest-quality) representation supplies the
			// decoded samples; the others are boundary references.
			if sp.repName == in.Name {
				sp.samples = append(sp.samples, toTimeline(rt, in.Name))
			}
		}
	}
	for _, key := range order {
		sp := slots[key]
		if len(sp.samples) == 0 {
			continue
		}
		tt := media.TrackTimeline{
			TrackID:   sp.trackID,
			Kind:      sp.kind,
			Timescale: sp.ts,
			Samples:   sp.samples,
			StartUS:   sp.samples[0].StartMicros,
			EndUS:     sp.samples[len(sp.samples)-1].EndMicros,
		}
		tl.Tracks = append(tl.Tracks, tt)
	}

	// Boundary comparison: every non-carrier representation is checked
	// against the carrier for the same track group.
	for _, key := range order {
		sp := slots[key]
		carrier := sp.boundaries[0]
		for _, b := range sp.boundaries[1:] {
			checkConflict(tl, opts, "start", carrier.StartUS, b.StartUS, carrier, b)
			checkConflict(tl, opts, "end", carrier.EndUS, b.EndUS, carrier, b)
			checkConflict(tl, opts, "span", carrier.EndUS-carrier.StartUS, b.EndUS-b.StartUS, carrier, b)
			if carrier.SampleCount != b.SampleCount {
				delta := int64(carrier.SampleCount - b.SampleCount)
				if delta < 0 {
					delta = -delta
				}
				tl.Conflicts = append(tl.Conflicts, media.Conflict{
					Kind:      sp.kind,
					Track:     b.TrackID,
					Metric:    "sample_count",
					DeltaUS:   0,
					Threshold: 0,
					Detail:    fmt.Sprintf("carrier %s=%d, %s=%d", carrier.Representation, carrier.SampleCount, b.Representation, b.SampleCount),
				})
			}
		}
	}
	if len(tl.Tracks) == 0 {
		return nil, fmt.Errorf("%w: no carrier samples after fan-in", media.ErrUnrecoverable)
	}
	// Sort each track's samples by start time, keeping DTS ties stable.
	for i := range tl.Tracks {
		tr := &tl.Tracks[i]
		sort.SliceStable(tr.Samples, func(a, b int) bool {
			s, t := tr.Samples[a], tr.Samples[b]
			if s.StartMicros == t.StartMicros {
				return s.DTS < t.DTS
			}
			return s.StartMicros < t.StartMicros
		})
		tr.StartUS = tr.Samples[0].StartMicros
		tr.EndUS = tr.Samples[len(tr.Samples)-1].EndMicros
	}
	return tl, nil
}

func checkConflict(tl *media.Timeline, opts Options, metric string, a, b int64, carrier, other media.Boundary) {
	delta := a - b
	if delta < 0 {
		delta = -delta
	}
	limit := opts.BoundaryThresholdUS
	if limit <= 0 {
		limit = 2000
	}
	if delta <= limit {
		return
	}
	tl.Conflicts = append(tl.Conflicts, media.Conflict{
		Kind:      other.Kind,
		Track:     other.TrackID,
		Metric:    metric,
		DeltaUS:   delta,
		Threshold: limit,
		Detail:    fmt.Sprintf("carrier %s=%d us, %s=%d us", carrier.Representation, pick(metric, a), other.Representation, pick(metric, b)),
	})
}

func pick(metric string, v int64) int64 { return v }

func toTimeline(rt *recovery.RecoveredTrack, rep string) []media.TimelineSample {
	out := make([]media.TimelineSample, 0, len(rt.Samples))
	for _, s := range rt.Samples {
		out = append(out, media.TimelineSample{
			Sample:         s.Sample,
			Representation: rep,
			StartMicros:    s.StartMicros,
			EndMicros:      s.EndMicros,
			Drift:          rt.Drift,
		})
	}
	return out
}

// MaxBoundaryDelta is the worst observed boundary disagreement, ignoring the
// sample-count metric (which has no microsecond value).
func MaxBoundaryDelta(tl *media.Timeline) int64 {
	max := int64(0)
	for _, c := range tl.Conflicts {
		if c.Metric == "sample_count" {
			continue
		}
		if c.DeltaUS > max {
			max = c.DeltaUS
		}
	}
	return max
}

// BoundaryDelta is the largest start/end/span disagreement across boundaries
// for the requested kind, whether or not it crossed the threshold.
func BoundaryDelta(bounds []media.Boundary, kind media.TrackKind) int64 {
	var carriers []media.Boundary
	for _, b := range bounds {
		if b.Kind == kind {
			carriers = append(carriers, b)
		}
	}
	if len(carriers) < 2 {
		return 0
	}
	max := int64(0)
	d := func(a, b int64) int64 {
		if a > b {
			return a - b
		}
		return b - a
	}
	for i := 1; i < len(carriers); i++ {
		for _, pair := range [][2]int64{
			{carriers[0].StartUS, carriers[i].StartUS},
			{carriers[0].EndUS, carriers[i].EndUS},
			{carriers[0].EndUS - carriers[0].StartUS, carriers[i].EndUS - carriers[i].StartUS},
		} {
			if v := d(pair[0], pair[1]); v > max {
				max = v
			}
		}
	}
	return max
}
