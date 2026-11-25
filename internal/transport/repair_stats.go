package transport

// Finish settles every group and returns them in first-sequence order.
func (r *Repairer) Finish() []*Group {
	groups := make([]*Group, 0, len(r.groups))
	seen := map[*Group]bool{}
	for _, base := range r.order {
		g, ok := r.groups[base]
		if !ok || seen[g] {
			continue
		}
		seen[g] = true
		r.settle(g)
		groups = append(groups, g)
	}
	return groups
}

// RecoveredSeqs lists every reconstructed sequence across all groups.
func (r *Repairer) RecoveredSeqs() []uint32 {
	var out []uint32
	for _, base := range r.order {
		if g, ok := r.groups[base]; ok {
			out = append(out, g.RecoveredSeqs()...)
		}
	}
	return out
}

// Payload returns the bytes for a sequence, original or reconstructed.
func (r *Repairer) Payload(seq uint32) ([]byte, bool) {
	base := seq - seq%uint32(r.groupSize)
	if g, ok := r.groups[base]; ok {
		return g.Payload(seq)
	}
	return nil, false
}

// RecoveredPayloadPct is repaired/(repaired+unrecovered), 100 when nothing
// was lost.
func (r *Repairer) RecoveredPayloadPct() float64 {
	total := r.Recovered + r.Unrecovered
	if total == 0 {
		return 100
	}
	return float64(r.Recovered) * 100 / float64(total)
}
