// Package repair makes a track's sample timestamps monotonic and reports
// exactly what it changed: clock drift, timestamp wraparound and duplicate
// timestamps.
package repair

import (
	"fmt"
	"math"

	"mediad/internal/clock"
	"mediad/internal/model"
)

// Options configures timestamp repair.
type Options struct {
	// NominalFrameRate, when set, is used to detect drift: the observed
	// inter-sample spacing is compared against 1/rate.
	NominalFrameRate clock.Rational
	// WrapTicks is the modulus of the timestamp field (1<<32 for RTP) and
	// ClockRate the ticks per second. A zero WrapTicks disables wrap
	// handling.
	WrapTicks uint64
	ClockRate uint32
	// MaxDriftPPM bounds how far the observed rate may deviate before it is
	// rejected as unusable metadata. Zero disables the check.
	MaxDriftPPM float64
}

// Report describes one track's repair.
type Report struct {
	TrackID uint32 `json:"track_id"`
	// BeforeOffset and AfterOffset are the first original DTS and the last
	// repaired DTS.
	BeforeOffset clock.Rational `json:"before_offset"`
	AfterOffset  clock.Rational `json:"after_offset"`
	// AppliedOffset is the total timing shift applied to the last sample.
	AppliedOffset clock.Rational `json:"applied_offset"`
	// DriftPPM is the observed rate error before repair, in parts per
	// million. Positive means the media clock ran fast.
	DriftPPM float64 `json:"drift_ppm"`
	// Wraps counts detected timestamp wraparounds.
	Wraps int `json:"wraps"`
	// Duplicates counts timestamps that repeated the previous sample.
	Duplicates int `json:"duplicates"`
	// Adjusted counts timestamps nudged to keep the track strictly
	// increasing.
	Adjusted int      `json:"adjusted"`
	Notes    []string `json:"notes"`
}

// Timestamps repairs a copy of samples and reports what changed. The input
// slice is never modified.
func Timestamps(trackID uint32, samples []model.Sample, opts Options) ([]model.Sample, Report, error) {
	rep := Report{TrackID: trackID}
	if len(samples) == 0 {
		return nil, rep, model.New(model.KindInvalidInput, "repair.timestamps",
			"no samples to repair").WithTrack(trackID)
	}
	out := append([]model.Sample(nil), samples...)
	rep.BeforeOffset = out[0].DTS

	dts := make([]clock.Rational, len(out))
	for i := range out {
		dts[i] = out[i].DTS
	}

	// 1. Unwrap: a backward jump by roughly the timestamp modulus is a field
	// wrap, not a rewind. The shift is cumulative, so every sample after the
	// wrap stays on the same extended timeline; the wrap is counted once per
	// period crossed, not once per sample.
	if opts.WrapTicks > 0 && opts.ClockRate > 0 {
		mod := clock.Rational{Num: int64(opts.WrapTicks), Den: int64(opts.ClockRate)}
		shift := clock.Zero()
		for i := 1; i < len(dts); i++ {
			cand, err := dts[i].Add(shift)
			if err != nil {
				return nil, rep, model.Wrap(model.KindInvalidMedia, "repair.timestamps",
					fmt.Sprintf("sample %d timestamp overflow", i), err).WithTrack(trackID)
			}
			if dts[i-1].Less(cand) {
				dts[i] = cand
				continue
			}
			// Only treat a backward jump as a wrap when one period actually
			// repairs the ordering; otherwise it is a genuine rewind and is
			// left for the monotonicity pass.
			next, err := shift.Add(mod)
			if err != nil {
				continue
			}
			cand, err = dts[i].Add(next)
			if err != nil || !dts[i-1].Less(cand) {
				dts[i] = cand
				continue
			}
			shift = next
			dts[i] = cand
			rep.Wraps++
		}
		if rep.Wraps > 0 {
			rep.Notes = append(rep.Notes, fmt.Sprintf(
				"unwrapped %d timestamp field wrap(s) of %s seconds at %d ticks", rep.Wraps, mod.String(), opts.WrapTicks))
		}
	}

	// 2. Measure drift before adjusting anything. Drift is the relative
	// error between the observed span and the nominal frame count.
	if !opts.NominalFrameRate.IsZero() && len(dts) > 1 {
		span, err := dts[len(dts)-1].Sub(dts[0])
		if err == nil && span.Sign() > 0 {
			want, err := clock.FromInt(int64(len(dts) - 1)).Div(opts.NominalFrameRate)
			if err == nil && want.Sign() > 0 {
				diff, err := span.Sub(want)
				if err == nil {
					rep.DriftPPM = diff.Float64() / want.Float64() * 1e6
				}
			}
		}
		if opts.MaxDriftPPM > 0 && math.Abs(rep.DriftPPM) > opts.MaxDriftPPM {
			return nil, rep, model.New(model.KindInvalidMedia, "repair.timestamps",
				fmt.Sprintf("clock drift %.1f ppm exceeds limit %.1f ppm", rep.DriftPPM, opts.MaxDriftPPM)).WithTrack(trackID)
		}
	}

	// 3. Enforce strictly increasing DTS. A duplicate or backward step is
	// advanced by exactly one tick of the track's own time base, so the
	// original spacing is preserved everywhere else.
	tick := oneTick(dts[0])
	for i := 1; i < len(out); i++ {
		// A repeated timestamp is a duplicate only when the original wire
		// value matched its predecessor; a value already nudged into
		// monotonicity is counted as Adjusted, not Duplicate.
		if samples[i].DTS.Equal(samples[i-1].DTS) {
			rep.Duplicates++
		}
		if !dts[i-1].Less(dts[i]) {
			next, err := dts[i-1].Add(tick)
			if err != nil {
				return nil, rep, model.Wrap(model.KindInvalidMedia, "repair.timestamps",
					fmt.Sprintf("sample %d timestamp overflow", i), err).WithTrack(trackID)
			}
			dts[i] = next
			rep.Adjusted++
		}
	}

	// 4. Rebuild samples, keeping each PTS offset relative to its original
	// DTS so presentation order is not silently changed.
	for i := range out {
		off, err := out[i].PTS.Sub(samples[i].DTS)
		if err != nil {
			off = clock.Zero()
		}
		out[i].DTS = dts[i]
		if pts, err := dts[i].Add(off); err == nil {
			out[i].PTS = pts
		} else {
			out[i].PTS = dts[i]
		}
		if i+1 < len(out) {
			if d, err := dts[i+1].Sub(dts[i]); err == nil && d.Sign() > 0 {
				out[i].Duration = d
			}
		}
	}
	if n := len(out); n > 1 {
		out[n-1].Duration = out[n-2].Duration
	}

	rep.AfterOffset = out[len(out)-1].DTS
	if off, err := rep.AfterOffset.Sub(rep.BeforeOffset); err == nil {
		rep.AppliedOffset = off
	}
	if rep.Adjusted > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"advanced %d non-monotonic timestamp(s) by one tick (%s s)", rep.Adjusted, tick.String()))
	}
	if rep.Duplicates > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("resolved %d duplicate timestamp(s)", rep.Duplicates))
	}
	return out, rep, nil
}

// oneTick returns the smallest unit implied by r's denominator.
func oneTick(r clock.Rational) clock.Rational {
	if r.Den > 1 {
		return clock.Rational{Num: 1, Den: r.Den}
	}
	// Whole-second time base: fall back to microseconds; callers that need
	// finer resolution supply a fractional DTS.
	return clock.MustNew(1, 1_000_000)
}

// Track repairs a model.Track in place and returns the report.
func Track(t *model.Track, opts Options) (Report, error) {
	if opts.NominalFrameRate.IsZero() {
		opts.NominalFrameRate = t.FrameRate
	}
	samples, rep, err := Timestamps(t.ID, t.Samples, opts)
	if err != nil {
		return rep, err
	}
	t.Samples = samples
	if len(samples) > 0 {
		last, err := samples[len(samples)-1].End()
		if err == nil {
			if d, err := last.Sub(t.Start); err == nil {
				t.Duration = d
			}
		}
	}
	return rep, nil
}
