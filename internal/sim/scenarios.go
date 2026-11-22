package sim

import (
	"math/rand"
	"sort"

	"mediakernel/internal/clock"
)

// applyCorrupt scatters isolated drops, duplicates and reorders. Isolated
// drops leave occasional unrecoverable edge gaps so the corrupt scenario
// always reports non-zero loss.
func applyCorrupt(rng *rand.Rand, packets int, drops, dupes, reorders map[uint16]bool) {
	for i := 2; i < packets-2; i++ {
		roll := rng.Float64()
		seq := uint16(i)
		switch {
		case roll < 0.08:
			drops[seq] = true
		case roll < 0.14:
			dupes[seq] = true
		case roll < 0.19:
			reorders[seq] = true
		}
	}
	// Guarantee an internal burst beyond the interpolation reach (default
	// cap is five packets). It is bracketed by real packets so the loss is
	// detected, yet too long to reconstruct, yielding non-zero unrecoverable
	// samples on every seed.
	for k := 0; k < 7; k++ {
		drops[uint16(60+k)] = true
	}
}

// applyRecovery places short, fully bracketed loss runs that the recovery
// policies can reconstruct: single-packet silence fills plus a few two-packet
// interpolation bursts, never touching stream edges.
func applyRecovery(rng *rand.Rand, packets int, drops map[uint16]bool, tr *Trace) {
	runAt := func(start, length int) {
		for k := 0; k < length; k++ {
			drops[uint16(start+k)] = true
		}
		tr.BurstRanges = append(tr.BurstRanges,
			[2]uint16{uint16(start), uint16(start + length - 1)})
	}
	// Deterministic anchor runs plus seed-driven extras, always bracketed.
	runAt(8, 1)
	runAt(24, 2)
	runAt(48, 1)
	runAt(72, 3)
	for i := 90; i < packets-6; i += 12 {
		if rng.Float64() < 0.5 {
			length := 1 + rng.Intn(2) // 1..2, stays inside interpolation reach
			runAt(i, length)
			i += length
		}
	}
}

// applyBurst combines congestion bursts with duplicates and reordering to
// stress every pipeline stage at once.
func applyBurst(rng *rand.Rand, packets int, drops, dupes, reorders map[uint16]bool, tr *Trace) {
	burst := func(start, length int) {
		for k := 0; k < length; k++ {
			drops[uint16(start+k)] = true
		}
		tr.BurstRanges = append(tr.BurstRanges,
			[2]uint16{uint16(start), uint16(start + length - 1)})
	}
	burst(10, 4)
	burst(40, 2)
	burst(80, 6) // beyond interpolation reach -> partly unrecoverable
	for i := 5; i < packets-3; i++ {
		if drops[uint16(i)] {
			continue
		}
		roll := rng.Float64()
		switch {
		case roll < 0.07:
			dupes[uint16(i)] = true
		case roll < 0.13:
			reorders[uint16(i)] = true
		}
	}
}

func buildBandwidth(scenario string, rng *rand.Rand, p Params) []BandwidthSample {
	samples := make([]BandwidthSample, 0, 12)
	base := 64_000.0
	steps := 12
	for i := 0; i < steps; i++ {
		at := clock.Ticks(int64(i) * int64(p.Packets) / int64(steps) * int64(p.BaseInterval))
		multiplier := 1.0
		switch scenario {
		case ScenarioBurst, ScenarioCorrupt:
			// V-shaped congestion: drops then recovers.
			multiplier = 0.35 + 0.65*float64(abs(i-steps/2))/float64(steps/2)
		case ScenarioRecovery:
			multiplier = 0.6 + 0.4*float64(i)/float64(steps)
		default:
			multiplier = 0.95 + rng.Float64()*0.1
		}
		samples = append(samples, BandwidthSample{
			At: at, BytesPerSec: base * multiplier,
		})
	}
	return samples
}

func sortedDrops(drops map[uint16]bool, packets int) []uint16 {
	out := make([]uint16, 0, len(drops))
	for seq := range drops {
		out = append(out, seq)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortActions(actions []Action) {
	sort.SliceStable(actions, func(i, j int) bool {
		if actions[i].Arrival != actions[j].Arrival {
			return actions[i].Arrival < actions[j].Arrival
		}
		if actions[i].OriginalIndex != actions[j].OriginalIndex {
			return actions[i].OriginalIndex < actions[j].OriginalIndex
		}
		return actions[i].Kind < actions[j].Kind
	})
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
