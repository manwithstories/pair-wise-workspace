// Package clock unwraps 32-bit device tick counters using a half-range
// rule and fits a rational drift correction against wall-clock anchors.
package clock

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"

	"fieldcut/internal/ingest"
	"fieldcut/internal/media"
)

// Sample pairs a raw device tick with the corresponding wall time.
type Sample struct {
	Seq       int64
	Tick      uint64 // raw 32-bit counter value, may have wrapped
	Nanos     int64
	Unwrapped int64 // expanded device ticks, strictly increasing per track
}

// Fit is the drift model over device ticks. Slope is nanoseconds per tick
// stored exactly as a rational; Intercept is wall nanoseconds at tick 0.
type Fit struct {
	Slope      media.Rational // nanos per tick, denominator 1e9
	Intercept  int64
	PPM        float64
	MaxErrorMs float64
}

// Unwrap expands raw 32-bit ticks for one track. For each step the forward
// delta is taken modulo wrapAt; a delta at or beyond the half range cannot
// be told apart from a backward glitch and is reported with track and
// sequence. wrapAt is the exclusive modulus (2^32 for a full range).
func Unwrap(track string, samples []Sample, wrapAt int64) ([]Sample, error) {
	if wrapAt <= 0 || wrapAt > 1<<32 {
		return nil, media.Newf(media.KindClock, "clock.unwrap", track, -1,
			"wrap_at=%d outside (0,2^32]", wrapAt)
	}
	if len(samples) == 0 {
		return nil, media.New(media.KindClock, "clock.unwrap", track, -1, errors.New("no samples"))
	}
	mod := uint64(wrapAt)
	half := mod / 2
	out := make([]Sample, 0, len(samples))
	base := uint64(0)
	prevRaw := uint64(0)
	for i, s := range samples {
		raw := s.Tick
		if raw >= mod {
			return nil, media.Newf(media.KindClock, "clock.unwrap", track, s.Seq,
				"tick %d outside modulus %d", raw, mod)
		}
		if i == 0 {
			out = append(out, Sample{Seq: s.Seq, Tick: raw, Nanos: s.Nanos, Unwrapped: int64(raw)})
			prevRaw = raw
			continue
		}
		var delta uint64
		if raw >= prevRaw {
			delta = raw - prevRaw
		} else {
			delta = mod - prevRaw + raw
		}
		if delta == 0 {
			return nil, media.Newf(media.KindClock, "clock.unwrap", track, s.Seq,
				"repeated tick %d", raw)
		}
		if delta >= half {
			return nil, media.Newf(media.KindClock, "clock.unwrap", track, s.Seq,
				"ambiguous wrap: raw stepped %d -> %d, delta %d >= half range %d", prevRaw, raw, delta, half)
		}
		if raw < prevRaw {
			base += mod
		}
		if base+raw > math.MaxInt64 {
			return nil, media.Newf(media.KindClock, "clock.unwrap", track, s.Seq, "counter overflow")
		}
		out = append(out, Sample{Seq: s.Seq, Tick: raw, Nanos: s.Nanos, Unwrapped: int64(base + raw)})
		prevRaw = raw
	}
	for i := 1; i < len(out); i++ {
		if out[i].Unwrapped <= out[i-1].Unwrapped {
			return nil, media.Newf(media.KindClock, "clock.unwrap", track, out[i].Seq,
				"non-monotonic unwrapped tick %d after %d", out[i].Unwrapped, out[i-1].Unwrapped)
		}
	}
	return out, nil
}

// Marker identifies one anchor pair used for the least-squares fit.
type Marker struct {
	Track string
	Seq   int64
	Tick  int64
	Nanos int64
}

// FitDrift regresses wall nanoseconds on unwrapped ticks over anchors.
// nominal is the expected nanoseconds per tick (1000 for a 1 µs tick); the
// fitted slope carries the device drift. Slope is kept as an exact rational
// scaled by 1e9, never a float sum.
func FitDrift(anchors []Marker, nominal media.Rational) (Fit, error) {
	if len(anchors) < 2 {
		return Fit{}, media.New(media.KindClock, "clock.fit", "", -1, errors.New("need at least two anchors"))
	}
	if nominal.Num <= 0 || nominal.Den <= 0 {
		return Fit{}, media.Newf(media.KindClock, "clock.fit", "", -1, "bad nominal %s", nominal)
	}
	sorted := append([]Marker(nil), anchors...)
	sort.Slice(sorted, func(a, b int) bool {
		if sorted[a].Track != sorted[b].Track {
			return sorted[a].Track < sorted[b].Track
		}
		if sorted[a].Tick != sorted[b].Tick {
			return sorted[a].Tick < sorted[b].Tick
		}
		return sorted[a].Seq < sorted[b].Seq
	})
	// Center the regressors before accumulating. A 32-bit counter shifted
	// near 2^32 otherwise makes x*x sums lose most of their significant
	// digits in float64; centering keeps the fit stable to sub-microsecond.
	var mx, my float64
	for _, a := range sorted {
		mx += float64(a.Tick)
		my += float64(a.Nanos)
	}
	n := float64(len(sorted))
	mx /= n
	my /= n
	var cov, variance float64
	for _, a := range sorted {
		dx := float64(a.Tick) - mx
		dy := float64(a.Nanos) - my
		cov += dx * dy
		variance += dx * dx
	}
	if variance == 0 {
		return Fit{}, media.New(media.KindClock, "clock.fit", "", -1, errors.New("anchors share one tick; cannot fit slope"))
	}
	slope := cov / variance
	intercept := my - slope*mx
	if slope <= 0 || math.IsNaN(slope) || math.IsInf(slope, 0) {
		return Fit{}, media.Newf(media.KindClock, "clock.fit", "", -1, "non-physical slope %v", slope)
	}
	nominalFloat := nominal.Float()
	ppm := (slope/nominalFloat - 1) * 1e6
	maxErr := 0.0
	for _, a := range sorted {
		resid := float64(a.Nanos) - (slope*float64(a.Tick) + intercept)
		if math.Abs(resid) > maxErr {
			maxErr = math.Abs(resid)
		}
	}
	scaled := int64(math.Round(slope * 1e9))
	if scaled <= 0 {
		return Fit{}, media.Newf(media.KindClock, "clock.fit", "", -1, "slope rounds to %d", scaled)
	}
	return Fit{
		Slope:      media.NewRational(scaled, 1e9),
		Intercept:  int64(math.Round(intercept)),
		PPM:        ppm,
		MaxErrorMs: maxErr / 1e6,
	}, nil
}

// MapWallNanos maps an unwrapped device tick to wall nanoseconds through
// the exact rational fit, truncating the sub-nanosecond remainder. The
// product is evaluated with big.Int so full 32-bit counter ranges and any
// slope denominator cannot overflow int64.
func (f Fit) MapWallNanos(tick int64) int64 {
	prod := new(big.Int).Mul(big.NewInt(f.Slope.Num), big.NewInt(tick))
	prod.Quo(prod, big.NewInt(f.Slope.Den))
	return prod.Int64() + f.Intercept
}

// MapWall maps an unwrapped device tick to exact wall time.
func (f Fit) MapWall(tick int64) media.Rational {
	return media.NewRational(f.MapWallNanos(tick), 1)
}

// AnchorsFromEntries pairs manifest entries of one track into anchor markers.
func AnchorsFromEntries(track string, entries []ingest.Entry) ([]Marker, error) {
	var anchors []Marker
	for _, e := range entries {
		if e.Nanos < 0 || e.Tick < 0 {
			return nil, media.Newf(media.KindClock, "clock.anchors", track, e.Seq, "negative tick or nanos")
		}
		anchors = append(anchors, Marker{Track: track, Seq: e.Seq, Tick: e.Tick, Nanos: e.Nanos})
	}
	if len(anchors) < 2 {
		return nil, media.Newf(media.KindClock, "clock.anchors", track, -1, "need two anchors, got %d", len(anchors))
	}
	return anchors, nil
}

// String renders the fit compactly for terminal output.
func (f Fit) String() string {
	return fmt.Sprintf("slope=%s ppm=%.3f max_error_ms=%.3f", f.Slope, f.PPM, f.MaxErrorMs)
}
