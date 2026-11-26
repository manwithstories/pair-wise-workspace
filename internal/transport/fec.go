// Package transport restores delivery order and integrity of synthetic RTP
// packets: bounded-window reordering, duplicate suppression, per-group XOR
// erasure recovery and precise gap reporting. It never fabricates media data:
// unrecovered losses are emitted as explicit gaps.
package transport

import (
	"mediaflow/internal/fixture"
)

// Gap records an unrecovered packet loss with exact sequence identity and the
// bounded microsecond window in which the missing media belonged.
type Gap struct {
	Track       uint32
	FECGroup    int
	SeqFrom     uint32 // first missing expanded sequence
	SeqTo       uint32 // last missing expanded sequence (inclusive)
	Count       int
	StartUS     int64 // conservative lower bound on the missing time range
	EndUS       int64 // conservative upper bound
	Recoverable bool  // true when a single parity packet could have helped
}

// groupState tracks one FEC block: up to GroupSize media packets plus parity.
type groupState struct {
	track  uint32
	group  int
	media  map[int]fixture.Packet // slot index within group -> packet
	parity *fixture.Packet
	// missing slots detected in arrival order.
	missing []int
	// emitted groups are dropped from the resolver.
	closed bool
}

// FECResolver resolves FEC groups for one track.
type FECResolver struct {
	track       uint32
	groups      map[int]*groupState
	maxGroupAge int
	// ordered group indices retained for deterministic eviction.
	seen []int
}

// NewFECResolver builds a resolver for a single track.
func NewFECResolver(track uint32) *FECResolver {
	return &FECResolver{track: track, groups: map[int]*groupState{}, maxGroupAge: 32}
}

func (r *FECResolver) state(group int) *groupState {
	g, ok := r.groups[group]
	if !ok {
		g = &groupState{track: r.track, group: group, media: map[int]fixture.Packet{}}
		r.groups[group] = g
		r.seen = append(r.seen, group)
	}
	return g
}

// AddMedia registers an arrived media packet at its group slot.
func (r *FECResolver) AddMedia(p fixture.Packet, slot int) {
	g := r.state(p.FECGroup)
	g.media[slot] = p
}

// AddParity registers the group parity packet.
func (r *FECResolver) AddParity(p fixture.Packet) {
	g := r.state(p.FECGroup)
	cp := p
	g.parity = &cp
}

// MarkMissing records a slot whose sequence never arrived.
func (r *FECResolver) MarkMissing(group, slot int) {
	g := r.state(group)
	for _, s := range g.missing {
		if s == slot {
			return
		}
	}
	g.missing = append(g.missing, slot)
}

// ready reports whether a group can be finalized without waiting further:
//   - all GroupSize slots accounted for AND no loss (parity irrelevant), or
//   - all slots accounted for, loss occurred, and the parity packet has
//     arrived (so recovery can run or an unrecoverable gap be confirmed).
//
// A sequence-complete group still missing its parity packet is held open so a
// late parity packet can recover the loss instead of being turned into a gap.
func (g *groupState) ready() bool {
	if len(g.media)+len(g.missing) != fixture.FECGroupSize {
		return false
	}
	if len(g.missing) == 0 {
		return true
	}
	return g.parity != nil
}

// drainClose finalizes every resolved group and returns recovered packets,
// normally delivered packets (in slot order) and unrecovered gaps.
func (r *FECResolver) drainClose(force bool) (out []fixture.Packet, gaps []Gap) {
	for _, idx := range r.seen {
		g := r.groups[idx]
		if g.closed {
			continue
		}
		if !force && !g.ready() {
			continue
		}
		g.closed = true
		pkts, gap := r.finalize(g)
		out = append(out, pkts...)
		if gap != nil {
			gaps = append(gaps, *gap)
		}
	}
	// Evict closed groups.
	kept := r.seen[:0]
	for _, idx := range r.seen {
		if r.groups[idx].closed {
			delete(r.groups, idx)
		} else {
			kept = append(kept, idx)
		}
	}
	r.seen = kept
	return out, gaps
}

// finalize rebuilds exactly-one-loss groups from parity, emits present media
// in slot order and turns any remaining loss into a precise gap.
func (r *FECResolver) finalize(g *groupState) ([]fixture.Packet, *Gap) {
	// Single missing slot + parity: reconstruct via XOR. Never fabricate when
	// more than one slot is missing or parity is absent.
	if len(g.missing) == 1 && g.parity != nil {
		slot := g.missing[0]
		rebuilt := rebuildXOR(g, slot)
		g.media[slot] = rebuilt
		g.missing = nil
	}

	var out []fixture.Packet
	for slot := 0; slot < fixture.FECGroupSize; slot++ {
		if p, ok := g.media[slot]; ok {
			out = append(out, p)
		}
	}
	if len(g.missing) == 0 {
		return out, nil
	}
	return out, r.buildGap(g)
}

// rebuildXOR reconstructs one missing payload: missing = parity XOR (XOR of
// all arrived media). Sequence/timing metadata is taken from the parity frame
// with the Recovered flag set so downstream stages know the source.
func rebuildXOR(g *groupState, missingSlot int) fixture.Packet {
	var parity fixture.Packet
	if g.parity != nil {
		parity = *g.parity
	}
	payload := append([]byte(nil), parity.Payload...)
	for slot, p := range g.media {
		_ = slot
		for j, b := range p.Payload {
			if j < len(payload) {
				payload[j] ^= b
			}
		}
	}
	return fixture.Packet{
		Track:     g.track,
		Seq:       parity.Seq,
		RawTS:     parity.RawTS,
		TrueUS:    parity.TrueUS,
		Duration:  parity.Duration,
		Payload:   payload,
		FECGroup:  g.group,
		Recovered: true,
	}
}

// buildGap assembles the exact unrecovered loss record. Time bounds use the
// neighboring present packets so the gap is conservative, never invented.
func (r *FECResolver) buildGap(g *groupState) *Gap {
	missing := append([]int(nil), g.missing...)
	first, last := missing[0], missing[len(missing)-1]
	var startUS, endUS int64
	// Lower bound: timestamp of the slot just before the missing run.
	if prev, ok := g.media[first-1]; ok && first > 0 {
		startUS = prev.TrueUS
	} else if g.parity != nil {
		startUS = g.parity.TrueUS
	}
	// Upper bound: timestamp of the slot just after the missing run.
	if next, ok := g.media[last+1]; ok && last < fixture.FECGroupSize-1 {
		endUS = next.TrueUS
	} else if g.parity != nil {
		endUS = g.parity.TrueUS
	}
	seqFrom := uint32(g.group*fixture.FECGroupSize + first)
	seqTo := uint32(g.group*fixture.FECGroupSize + last)
	return &Gap{
		Track:       g.track,
		FECGroup:    g.group,
		SeqFrom:     seqFrom,
		SeqTo:       seqTo,
		Count:       len(missing),
		StartUS:     startUS,
		EndUS:       endUS,
		// A single-slot loss is the only case a parity packet could rebuild.
		Recoverable: len(missing) == 1,
	}
}

// Flush forces resolution of all buffered groups (end of stream).
func (r *FECResolver) Flush() ([]fixture.Packet, []Gap) {
	return r.drainClose(true)
}

// Drain finalizes only groups that are fully resolved.
func (r *FECResolver) Drain() ([]fixture.Packet, []Gap) {
	return r.drainClose(false)
}
