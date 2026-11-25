// Package clock converts per-track integer timestamps onto one exact rational
// archive timeline. It corrects measured clock drift and 32-bit counter
// wraparound using explicit, conservative rules; anything outside the rules
// is rejected rather than guessed.
package clock

import (
	"fmt"
	"math"

	"mediaarchive/internal/demux"
	"mediaarchive/internal/model"
)

// MaxDriftPPM is the inclusive drift magnitude the archive will correct.
// A device clock off by more than 250 ppm cannot be trusted for timeline
// reconstruction and the run must fail for manual review.
const MaxDriftPPM = 250

// NormalizedSample is a demuxed sample with its time expressed on the
// unified seconds timeline. All arithmetic stays in integer rationals.
type NormalizedSample struct {
	demux.Sample
	// DTSNorm is the corrected decode time in seconds since the archive
	// origin (the earliest mapped track start).
	DTSNorm model.Rational
	// DurNorm is the corrected sample duration (VFR: one value per sample).
	DurNorm model.Rational
	// WrapCount is the number of counter wraps observed before this sample.
	WrapCount uint64
}

// Corrector handles one track's timeline. It is stateful: wrap detection
// relies on the previous raw timestamp.
type Corrector struct {
	trackID   uint32
	timescale uint32
	driftPPM  int32
	wrapUnits uint32 // counter period; 0 means no wrap is legal
	start     uint64
	lastRaw   uint64
	hasLast   bool
	wraps     uint64
	// origin lets multiple tracks share one archive timeline.
	origin model.Rational
}

// NewCorrector validates the declared anomalies. |drift| > MaxDriftPPM is an
// unrecoverable clock error; a zero timescale is rejected as unusable.
func NewCorrector(meta demux.TrackMeta, origin model.Rational) (*Corrector, error) {
	if meta.Timescale == 0 {
		return nil, model.NewError(model.StageClock,
			fmt.Sprintf("track %d declares zero timescale", meta.ID)).WithTrack(int32(meta.ID))
	}
	if abs32(meta.DriftPPM) > MaxDriftPPM {
		return nil, model.NewError(model.StageClock,
			fmt.Sprintf("track %d drift %d ppm exceeds +/-%d ppm, correction refused",
				meta.ID, meta.DriftPPM, MaxDriftPPM)).WithTrack(int32(meta.ID))
	}
	return &Corrector{
		trackID:   meta.ID,
		timescale: meta.Timescale,
		driftPPM:  meta.DriftPPM,
		wrapUnits: meta.WrapUnits,
		start:     meta.StartTime,
		origin:    origin,
	}, nil
}

// Apply normalizes one raw sample timestamp and duration.
//
// Wrap rule: when wrapUnits > 0, a backward jump is accepted only if the
// previous value is in the upper half of the period and the new value in the
// lower half; anything else is ambiguous and aborts. When wrapUnits == 0,
// any backward jump aborts. VFR durations are corrected independently with
// the same per-track drift factor.
func (c *Corrector) Apply(s demux.Sample) (NormalizedSample, error) {
	if s.TrackID != c.trackID {
		return NormalizedSample{}, model.NewError(model.StageClock,
			fmt.Sprintf("corrector for track %d received track %d", c.trackID, s.TrackID)).
			WithTrack(int32(s.TrackID))
	}
	raw := s.DTS
	if c.hasLast && raw < c.lastRaw {
		if c.wrapUnits == 0 {
			return NormalizedSample{}, model.NewError(model.StageClock,
				fmt.Sprintf("track %d timestamp went backwards with no wrap period: %d -> %d",
					c.trackID, c.lastRaw, raw)).
				WithTrack(int32(c.trackID)).WithSample(s.Seq)
		}
		period := uint64(c.wrapUnits)
		half := period / 2
		if c.lastRaw < half || raw > half {
			return NormalizedSample{}, model.NewError(model.StageClock,
				fmt.Sprintf("track %d wrap cannot be decided: last=%d raw=%d period=%d",
					c.trackID, c.lastRaw, raw, period)).
				WithTrack(int32(c.trackID)).WithSample(s.Seq)
		}
		if c.lastRaw-raw > period {
			return NormalizedSample{}, model.NewError(model.StageClock,
				fmt.Sprintf("track %d backward jump larger than one period", c.trackID)).
				WithTrack(int32(c.trackID)).WithSample(s.Seq)
		}
		c.wraps++
	}
	c.lastRaw = raw
	c.hasLast = true

	unwrapped := c.wraps*uint64(c.wrapUnits) + raw
	if unwrapped < c.start {
		return NormalizedSample{}, model.NewError(model.StageClock,
			fmt.Sprintf("track %d timestamp %d before declared start %d", c.trackID, raw, c.start)).
			WithTrack(int32(c.trackID)).WithSample(s.Seq)
	}
	// Exact drift factor: a fast clock (positive ppm) over-counts units, so
	// true time = recorded * (1e6-ppm)/1e6.
	factor := model.NewRational(int64(1_000_000-c.driftPPM), 1_000_000)
	// Absolute corrected time, then shifted onto the shared timeline whose
	// zero is the earliest corrected track start (c.origin).
	absolute := model.NewRational(int64(unwrapped), int64(c.timescale)).Mul(factor)
	dur := model.NewRational(int64(s.Duration), int64(c.timescale)).Mul(factor)
	return NormalizedSample{
		Sample:    s,
		DTSNorm:   absolute.Sub(c.origin),
		DurNorm:   dur,
		WrapCount: c.wraps,
	}, nil
}

// Normalizer owns one corrector per mapped track and computes the shared
// archive origin (earliest corrected start).
type Normalizer struct {
	correctors map[uint32]*Corrector
}

// NewNormalizer builds correctors for every mapped track. The origin is the
// minimum corrected start time, so non-zero track starts line up correctly.
func NewNormalizer(tracks []demux.TrackMeta, mapped []uint32) (*Normalizer, error) {
	want := map[uint32]bool{}
	for _, id := range mapped {
		want[id] = true
	}
	// First pass with a zero origin to obtain corrected start times.
	tmp := map[uint32]*Corrector{}
	origin := model.NewRational(0, 1)
	first := true
	for _, meta := range tracks {
		if !want[meta.ID] {
			continue
		}
		cr, err := NewCorrector(meta, model.NewRational(0, 1))
		if err != nil {
			return nil, err
		}
		start := model.NewRational(int64(meta.StartTime), int64(meta.Timescale)).
			Mul(model.NewRational(int64(1_000_000-meta.DriftPPM), 1_000_000))
		if first || start.Cmp(origin) < 0 {
			origin = start
			first = false
		}
		tmp[meta.ID] = cr
	}
	if len(tmp) == 0 {
		return nil, model.NewError(model.StageClock, "no mapped tracks to normalize")
	}
	// Rebuild with the shared origin.
	n := &Normalizer{correctors: map[uint32]*Corrector{}}
	for _, meta := range tracks {
		if cr, ok := tmp[meta.ID]; ok {
			cr.origin = origin
			n.correctors[meta.ID] = cr
		}
	}
	return n, nil
}

// Apply routes a sample to its track corrector.
func (n *Normalizer) Apply(s demux.Sample) (NormalizedSample, error) {
	cr, ok := n.correctors[s.TrackID]
	if !ok {
		return NormalizedSample{}, model.NewError(model.StageClock,
			fmt.Sprintf("no corrector for track %d", s.TrackID)).WithTrack(int32(s.TrackID))
	}
	return cr.Apply(s)
}

// Origin reports the shared archive origin in corrected seconds.
func (n *Normalizer) Origin() model.Rational {
	for _, cr := range n.correctors {
		return cr.origin
	}
	return model.Rational{}
}

func abs32(v int32) int32 {
	if v == math.MinInt32 {
		return math.MaxInt32
	}
	if v < 0 {
		return -v
	}
	return v
}
