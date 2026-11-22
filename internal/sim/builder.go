package sim

import (
	"math/rand"

	"mediakernel/internal/clock"
)

// Scenario names accepted by the simulator and CLI.
const (
	ScenarioNominal  = "nominal"
	ScenarioCorrupt  = "corrupt"
	ScenarioRecovery = "recovery"
	ScenarioBurst    = "burst"
)

// ValidScenario reports whether name is a supported scenario.
func ValidScenario(name string) bool {
	switch name {
	case ScenarioNominal, ScenarioCorrupt, ScenarioRecovery, ScenarioBurst:
		return true
	default:
		return false
	}
}

// Build constructs the deterministic trace for (scenario, seed).
func Build(scenario string, seed int64, p Params) (*Trace, error) {
	if !ValidScenario(scenario) {
		return nil, &ScenarioError{Name: scenario}
	}
	if p.Packets <= 0 || p.FramesPerPacket <= 0 || p.BaseInterval <= 0 {
		p = DefaultParams()
	}
	rng := rand.New(rand.NewSource(seed))
	tr := &Trace{Scenario: scenario, Seed: seed}

	// First lay down the ideal stream with mild jitter on arrivals.
	type planned struct {
		seq       uint16
		mediaTime clock.Ticks
		arrival   clock.Ticks
	}
	items := make([]planned, p.Packets)
	for i := 0; i < p.Packets; i++ {
		media := clock.Ticks(i) * p.BaseInterval
		jitter := clock.Milliseconds(int64(rng.Intn(9)) - 4) // -4..+4 ms
		items[i] = planned{
			seq:       uint16(i),
			mediaTime: media,
			arrival:   media + clock.Milliseconds(60) + jitter,
		}
	}

	drops := map[uint16]bool{}
	dupes := map[uint16]bool{}
	reorders := map[uint16]bool{}

	switch scenario {
	case ScenarioNominal:
		// No impairments; jitter alone.
	case ScenarioCorrupt:
		applyCorrupt(rng, p.Packets, drops, dupes, reorders)
	case ScenarioRecovery:
		applyRecovery(rng, p.Packets, drops, tr)
	case ScenarioBurst:
		applyBurst(rng, p.Packets, drops, dupes, reorders, tr)
	}

	tr.Bandwidth = buildBandwidth(scenario, rng, p)
	tr.LostSeqs = sortedDrops(drops, p.Packets)

	// Emit actions in arrival order; reordering swaps arrival timestamps so
	// the action sequence genuinely leaves wire order.
	all := make([]Action, 0, p.Packets+len(dupes))
	for i := range items {
		it := items[i]
		if reorders[it.seq] && i+1 < len(items) {
			items[i].arrival, items[i+1].arrival = items[i+1].arrival, items[i].arrival
			it = items[i]
		}
		if drops[it.seq] {
			all = append(all, Action{
				Kind: ActDrop, Stream: p.AudioSSRC, Seq: it.seq,
				MediaTime: it.mediaTime, Arrival: it.arrival, OriginalIndex: i,
			})
			continue
		}
		kind := ActDeliver
		if reorders[it.seq] {
			kind = ActReorder
		}
		all = append(all, Action{
			Kind: kind, Stream: p.AudioSSRC, Seq: it.seq,
			MediaTime: it.mediaTime, Arrival: it.arrival, OriginalIndex: i,
		})
		if dupes[it.seq] {
			all = append(all, Action{
				Kind: ActDuplicate, Stream: p.AudioSSRC, Seq: it.seq,
				MediaTime:     it.mediaTime,
				Arrival:       it.arrival + clock.Milliseconds(3),
				OriginalIndex: i,
			})
		}
	}
	// Reorder actions by arrival to emulate the wire, keeping a stable tie
	// break on (arrival, original index, kind) for determinism.
	sortActions(all)
	tr.Actions = append(tr.Actions, all...)
	return tr, nil
}

// ScenarioError describes an unknown scenario name.
type ScenarioError struct{ Name string }

func (e *ScenarioError) Error() string {
	return "sim: unknown scenario " + e.Name
}
