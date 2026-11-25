package transport

import (
	"fmt"

	"avcheck/internal/media"
)

// maxRetainedGroups bounds how many repair groups stay resident, so a long
// run cannot grow repair memory without limit.
const maxRetainedGroups = 256

// Group is one XOR repair group: a fixed run of consecutive sequence numbers.
// At most one lost slot is recoverable; two or more leave an explicit gap.
type Group struct {
	Stream   uint16
	Ordinal  uint32
	FirstSeq uint32
	Size     int
	payload  [][]byte
	received []bool
	lost     []bool
	resolved []bool
	parity   []byte
	settled  bool
	// expect is the number of real slots in this group. It equals Size for
	// full groups and is smaller for the final, partial group; parity
	// headers carry the authoritative value.
	expect int
}

// RecoveredSeqs lists the slots reconstructed in this group.
func (g *Group) RecoveredSeqs() []uint32 {
	var out []uint32
	for i := range g.resolved {
		if g.resolved[i] {
			out = append(out, g.FirstSeq+uint32(i))
		}
	}
	return out
}

// LostSeqs lists the slots still unrecovered in this group.
func (g *Group) LostSeqs() []uint32 {
	var out []uint32
	for i := range g.lost {
		if g.lost[i] && !g.resolved[i] {
			out = append(out, g.FirstSeq+uint32(i))
		}
	}
	return out
}

// Payload returns the original or reconstructed bytes of a slot.
func (g *Group) Payload(seq uint32) ([]byte, bool) {
	d := SeqDiff(seq, g.FirstSeq)
	if d < 0 || d >= int32(g.Size) {
		return nil, false
	}
	if !g.received[d] {
		return nil, false
	}
	return g.payload[d], true
}

// Repairer builds XOR groups and resolves single-packet losses. Grouping is
// anchored to absolute sequence boundaries, so reordered packets land in the
// same group regardless of arrival order. Accounting happens once, at
// Finish, from the final state of every group.
type Repairer struct {
	groupSize   int
	faults      *media.Faults
	stream      uint16
	groups      map[uint32]*Group
	order       []uint32
	ordinal     uint32
	Recovered   int
	Unrecovered int
	Groups      int
}

// NewRepairer creates a repairer for one stream.
func NewRepairer(stream uint16, groupSize int, faults *media.Faults) (*Repairer, error) {
	if groupSize < 2 || groupSize > 256 {
		return nil, fmt.Errorf("transport: repair group size %d outside [2,256]", groupSize)
	}
	if faults == nil {
		faults = &media.Faults{}
	}
	return &Repairer{
		groupSize: groupSize,
		faults:    faults,
		stream:    stream,
		groups:    make(map[uint32]*Group),
	}, nil
}

// Add indexes one received payload; duplicates are ignored.
func (r *Repairer) Add(pkt Packet) {
	g := r.groupFor(pkt.Seq)
	if g == nil {
		return
	}
	slot := int(SeqDiff(pkt.Seq, g.FirstSeq))
	if slot < 0 || slot >= g.Size || g.received[slot] {
		return
	}
	g.payload[slot] = append([]byte(nil), pkt.Payload...)
	g.received[slot] = true
}

// MarkLost reserves a slot for a sequence proven missing.
func (r *Repairer) MarkLost(seq uint32) {
	g := r.groupFor(seq)
	if g == nil {
		return
	}
	slot := int(SeqDiff(seq, g.FirstSeq))
	if slot < 0 || slot >= g.Size || g.received[slot] {
		return
	}
	g.lost[slot] = true
}

// FeedParity applies XOR parity to the group named by h. The header's missing
// count identifies slots that never arrived, so the repairer does not have to
// be told about them separately. A group with exactly one missing slot and
// every other slot present is reconstructed; the return value reports that.
func (r *Repairer) FeedParity(h ParityHeader, parity []byte) bool {
	g := r.groupFor(h.BaseSeq)
	if g == nil || len(parity) == 0 {
		return false
	}
	g.parity = append([]byte(nil), parity...)
	n := int(h.GroupSize)
	if n <= 0 || n > g.Size {
		n = g.Size
	}
	g.expect = n
	if h.Missing > 0 {
		for i := 0; i < n; i++ {
			if !g.received[i] {
				g.lost[i] = true
			}
		}
	}
	return r.resolve(g)
}

// resolve reconstructs the single missing slot when the group is fully
// accounted for. With two or more losses it records the repair overflow and
// leaves every gap explicit.
func (r *Repairer) resolve(g *Group) bool {
	if len(g.parity) == 0 {
		return false
	}
	missing := -1
	lost := 0
	present := 0
	for i := range g.lost {
		if g.received[i] {
			present++
		} else if g.lost[i] {
			lost++
			missing = i
		}
	}
	if lost == 0 {
		return false
	}
	if lost > 1 {
		r.faults.Addf(media.LevelDegraded, media.CodeRepairOverflow, uint64(r.stream), int64(g.FirstSeq),
			"group %d lost %d packets; only one is recoverable", g.Ordinal, lost)
		return false
	}
	size := g.Size
	if g.expect > 0 && g.expect < size {
		size = g.expect
	}
	if present+lost != size {
		// Not all other slots arrived, so XOR cannot isolate the loss.
		return false
	}
	rec := make([]byte, len(g.parity))
	copy(rec, g.parity)
	for i := range g.payload {
		if i == missing || !g.received[i] {
			continue
		}
		for j := 0; j < len(rec) && j < len(g.payload[i]); j++ {
			rec[j] ^= g.payload[i][j]
		}
	}
	g.payload[missing] = rec
	g.received[missing] = true
	g.resolved[missing] = true
	seq := g.FirstSeq + uint32(missing)
	r.faults.Addf(media.LevelRecovered, media.CodePacketRecovered, uint64(r.stream), int64(seq),
		"recovered sequence %d via XOR parity", seq)
	return true
}

// groupFor returns the group containing seq, creating it on first sight.
// Old groups are pruned beyond maxRetainedGroups.
func (r *Repairer) groupFor(seq uint32) *Group {
	base := seq - seq%uint32(r.groupSize)
	if g, ok := r.groups[base]; ok {
		return g
	}
	g := &Group{
		Stream:   r.stream,
		Ordinal:  r.ordinal,
		FirstSeq: base,
		Size:     r.groupSize,
		payload:  make([][]byte, r.groupSize),
		received: make([]bool, r.groupSize),
		lost:     make([]bool, r.groupSize),
		resolved: make([]bool, r.groupSize),
	}
	r.ordinal++
	r.groups[base] = g
	r.order = append(r.order, base)
	r.prune()
	return g
}

// prune drops the oldest groups once the retention bound is exceeded. A
// dropped group is settled into the counters first so nothing is lost.
func (r *Repairer) prune() {
	for len(r.order) > maxRetainedGroups {
		base := r.order[0]
		r.order = r.order[1:]
		if g, ok := r.groups[base]; ok {
			r.settle(g)
			delete(r.groups, base)
		}
	}
}

// settle adds one group's outcome to the counters. It is idempotent.
func (r *Repairer) settle(g *Group) {
	if g.settled {
		return
	}
	rec := len(g.RecoveredSeqs())
	lost := len(g.LostSeqs())
	if rec == 0 && lost == 0 && len(g.parity) == 0 {
		return
	}
	r.Groups++
	r.Recovered += rec
	r.Unrecovered += lost
	for _, seq := range g.LostSeqs() {
		r.faults.Addf(media.LevelDegraded, media.CodePacketGap, uint64(r.stream),
			int64(seq), "sequence %d unrepaired", seq)
	}
	g.settled = true
}
