// Package clock normalizes capture timestamps: it unwraps integer counter
// wrap-around, estimates the capture clock drift and maps variable frame
// rate (VFR) sample times onto one shared timeline.
package clock

import (
	"fmt"
	"math"
	"sort"

	"medpipe/internal/model"
)

// UnwrapOptions configures counter wrap recovery.
type UnwrapOptions struct {
	// Bits is the raw counter width (8, 16, 32 or 64). Zero means 64.
	Bits uint8
	// NominalDelta is the expected raw tick step between frames. It is only
	// used to sanity-check the forward gap after a wrap.
	NominalDelta uint64
}

// UnwrapResult is the normalized view of one track's raw counter.
type UnwrapResult struct {
	Bits      uint8
	Modulus   uint64
	Wraps     int
	Raw       []uint64
	Unwrapped []uint64
	Deltas    []uint64
	DriftPPM  float64
}

// Unwrap converts a monotonically increasing modulo-N counter into an
// absolute tick sequence. Each forward step must be smaller than half the
// counter range; a larger step is genuinely ambiguous (it could be a small
// backward move or a missed wrap) and yields a typed error rather than a
// guessed value.
func Unwrap(raw []uint64, opt UnwrapOptions) (*UnwrapResult, error) {
	bits := opt.Bits
	if bits == 0 {
		bits = 64
	}
	if bits != 8 && bits != 16 && bits != 32 && bits != 64 {
		return nil, model.New(model.KindInput, "clock.Unwrap", fmt.Sprintf("unsupported counter width %d", bits))
	}
	res := &UnwrapResult{Bits: bits, Raw: append([]uint64(nil), raw...)}
	if bits == 64 {
		res.Modulus = 0 // unreachable; 64-bit counters are not wrapped
	} else {
		res.Modulus = uint64(1) << bits
	}
	if len(raw) == 0 {
		return res, nil
	}
	if bits != 64 {
		for i, v := range raw {
			if v >= res.Modulus {
				return nil, model.New(model.KindCorrupt, "clock.Unwrap",
					fmt.Sprintf("frame %d raw timestamp %d exceeds %d-bit counter range", i, v, bits))
			}
		}
	}

	res.Unwrapped = make([]uint64, len(raw))
	res.Unwrapped[0] = raw[0]
	res.Deltas = make([]uint64, len(raw)-1)
	for i := 1; i < len(raw); i++ {
		var delta uint64
		if bits == 64 {
			if raw[i] < raw[i-1] {
				return nil, model.New(model.KindAmbiguous, "clock.Unwrap",
					fmt.Sprintf("64-bit timestamp regressed at frame %d (%d -> %d)", i, raw[i-1], raw[i]))
			}
			delta = raw[i] - raw[i-1]
		} else {
			// Modular forward distance. For a healthy stream this is the
			// small positive step, including the step that crosses zero.
			delta = (raw[i] + res.Modulus - raw[i-1]%res.Modulus) % res.Modulus
		}
		if delta == 0 {
			return nil, model.New(model.KindAmbiguous, "clock.Unwrap",
				fmt.Sprintf("non-advancing timestamp at frame %d (raw %d)", i, raw[i]))
		}
		if bits != 64 && delta >= res.Modulus/2 {
			return nil, model.New(model.KindAmbiguous, "clock.Unwrap",
				fmt.Sprintf("frame %d step of %d ticks is half the %d-bit range or more; wrap ordering is undecidable", i, delta, bits))
		}
		if raw[i] < raw[i-1] {
			res.Wraps++
		}
		res.Deltas[i-1] = delta
		// Unwrapped code is the raw value plus one modulus per observed wrap.
		res.Unwrapped[i] = raw[i] + uint64(res.Wraps)*res.Modulus
		if bits == 64 {
			res.Unwrapped[i] = raw[i]
		}
	}
	res.DriftPPM = EstimateDriftPPM(res.Unwrapped, opt.NominalDelta)
	return res, nil
}

// EstimateDriftPPM estimates the relative frequency error of the capture
// counter in parts per million. Steps that depart from the nominal interval by
// more than 10% are treated as VFR outliers (a camera that switched rate) and
// excluded; the mean of the remaining integer steps recovers sub-tick drift
// that a median would quantize away. If no step is close to nominal, the
// median step is used so a wrong declared nominal value still yields a sane
// answer.
func EstimateDriftPPM(unwrapped []uint64, nominalDelta uint64) float64 {
	n := len(unwrapped)
	if n < 2 || nominalDelta == 0 {
		return 0
	}
	steps := make([]uint64, 0, n-1)
	for i := 1; i < n; i++ {
		if unwrapped[i] <= unwrapped[i-1] {
			continue
		}
		steps = append(steps, unwrapped[i]-unwrapped[i-1])
	}
	if len(steps) == 0 {
		return 0
	}
	lo := float64(nominalDelta) * 0.9
	hi := float64(nominalDelta) * 1.1
	var sum float64
	var kept int
	for _, s := range steps {
		if f := float64(s); f >= lo && f <= hi {
			sum += f
			kept++
		}
	}
	if kept == 0 {
		sorted := append([]uint64(nil), steps...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		median := sorted[len(sorted)/2]
		if median == 0 {
			return 0
		}
		return (float64(median)/float64(nominalDelta) - 1) * 1e6
	}
	mean := sum / float64(kept)
	return (mean/float64(nominalDelta) - 1) * 1e6
}

// CorrectedTicks removes the measured drift from an unwrapped tick value.
// It is intentionally a pure function of (tick, drift) so repeated
// normalization of the same input always produces identical output.
func CorrectedTicks(tick uint64, driftPPM float64) float64 {
	return float64(tick) / (1 + driftPPM/1e6)
}

// TickSeconds converts a corrected tick value to seconds using the track
// timescale's tick rate.
func TickSeconds(corrected float64, tickRate float64) float64 {
	if tickRate == 0 {
		return 0
	}
	return corrected / tickRate
}

// RoundTicks rounds a float tick value deterministically (half away from
// zero) so that timeline mapping has no platform-dependent rounding.
func RoundTicks(v float64) int64 {
	if v < 0 {
		return int64(math.Floor(v - 0.5))
	}
	return int64(math.Floor(v + 0.5))
}
