// Package merge turns the order-preserving worker output into the final
// deterministic output order.
//
// Packets are ordered by presentation time, then by stable track ID, then by
// per-track sequence number. Ordering is done with an exact temporal key so a
// variable frame rate is preserved: no average frame rate is ever assumed and
// no timestamp is ever synthesised. Packets with identical content are emitted
// once. A packet whose decode timestamp regresses within its track, or that
// carries no timestamp, is quarantined with a reason and never silently
// reordered into the output.
package merge

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"avpipe/internal/media"
)

// ErrCanceled is returned when the merge is canceled.
var ErrCanceled = errors.New("merge: canceled")

// Policy controls which anomalies are fatal.
type Policy struct {
	// AllowMissingTimestamp drops packets without a presentation timestamp
	// into quarantine instead of failing the run.
	AllowMissingTimestamp bool
	// AllowDTSRegression quarantines a decode-time regression instead of
	// failing the run.
	AllowDTSRegression bool
	// MaxQuarantine bounds the number of quarantined packets retained for
	// reporting. Zero uses 4096.
	MaxQuarantine int
	// MaxPackets bounds the number of packets retained in memory. Zero uses
	// 4,000,000.
	MaxPackets int
}

func (p Policy) normalize() Policy {
	if p.MaxQuarantine <= 0 {
		p.MaxQuarantine = 4096
	}
	if p.MaxPackets <= 0 {
		p.MaxPackets = 4_000_000
	}
	return p
}

// QuarantineReason categorises why a packet was withheld from output.
type QuarantineReason string

const (
	ReasonMissingTimestamp QuarantineReason = "missing_timestamp"
	ReasonDTSRegression    QuarantineReason = "dts_regression"
	ReasonLatePacket       QuarantineReason = "late_packet"
	ReasonDuplicate        QuarantineReason = "duplicate"
)

// Quarantined is one withheld packet and the reason.
type Quarantined struct {
	Packet media.Packet
	Reason QuarantineReason
	Detail string
}

// Stats summarises a merge.
type Stats struct {
	In          int
	Out         int
	Duplicates  int
	Quarantined int
	Reasons     map[QuarantineReason]int
}

// Merger accumulates packets and emits them in deterministic output order.
type Merger struct {
	policy Policy
	// byTrack holds each track's packets in the order they were accepted.
	byTrack map[string][]media.Packet
	// lastDTS records the highest DTS accepted per track.
	lastDTS map[string]int64
	seen    map[string]bool
	quarant []Quarantined
	stats   Stats
	order   []string // stable track ID encounter order, for deterministic tiebreak
	trackRank map[string]int
	closed  bool
}

// New builds a merger.
func New(policy Policy) *Merger {
	return &Merger{
		policy:   policy.normalize(),
		byTrack:  map[string][]media.Packet{},
		lastDTS:  map[string]int64{},
		seen:     map[string]bool{},
		trackRank: map[string]int{},
		stats:    Stats{Reasons: map[QuarantineReason]int{}},
	}
}

// Add accepts one packet. It returns false when the packet was quarantined.
func (m *Merger) Add(pkt media.Packet) (bool, error) {
	m.stats.In++
	if m.stats.In > m.policy.MaxPackets {
		return false, media.Errorf(media.KindPolicy, pkt.Origin, "merge.Add",
			"packet count exceeds limit %d", m.policy.MaxPackets)
	}
	// Content duplicates are dropped before any ordering decision.
	fp := pkt.Hash()
	if m.seen[fp] {
		m.stats.Duplicates++
		m.record(pkt, ReasonDuplicate, "identical content already emitted")
		return false, nil
	}
	// Missing presentation timestamp: never synthesise one.
	if !pkt.HasPTS() {
		if !m.policy.AllowMissingTimestamp {
			return false, media.Errorf(media.KindCorrupt, pkt.Origin, "merge.Add",
				"packet has no presentation timestamp")
		}
		m.record(pkt, ReasonMissingTimestamp, "no PTS")
		return false, nil
	}
	// Decode-time regressions are quarantined, never reordered.
	if pkt.HasDTS() {
		if last, ok := m.lastDTS[pkt.TrackID]; ok && pkt.DTS < last {
			if !m.policy.AllowDTSRegression {
				return false, media.Errorf(media.KindPolicy, pkt.Origin, "merge.Add",
					"decode timestamp regressed from %d to %d on track %s", last, pkt.DTS, pkt.TrackID)
			}
			m.record(pkt, ReasonDTSRegression,
				fmt.Sprintf("DTS %d < previous %d", pkt.DTS, last))
			return false, nil
		}
		m.lastDTS[pkt.TrackID] = pkt.DTS
	}
	m.seen[fp] = true
	if _, ok := m.trackRank[pkt.TrackID]; !ok {
		m.trackRank[pkt.TrackID] = len(m.order)
		m.order = append(m.order, pkt.TrackID)
	}
	m.byTrack[pkt.TrackID] = append(m.byTrack[pkt.TrackID], pkt)
	m.stats.Out++
	return true, nil
}

func (m *Merger) record(pkt media.Packet, reason QuarantineReason, detail string) {
	m.stats.Quarantined++
	m.stats.Reasons[reason]++
	if len(m.quarant) < m.policy.MaxQuarantine {
		m.quarant = append(m.quarant, Quarantined{Packet: pkt, Reason: reason, Detail: detail})
	}
}

// Sorted returns the deterministic output order: presentation time, then
// stable track ID, then per-track sequence.
func (m *Merger) Sorted() ([]media.Packet, error) {
	var all []media.Packet
	for _, id := range m.order {
		all = append(all, m.byTrack[id]...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		// Compare exact presentation times without float conversion.
		if c := compareTime(a, b); c != 0 {
			return c < 0
		}
		if a.TrackID != b.TrackID {
			return a.TrackID < b.TrackID
		}
		return a.Seq < b.Seq
	})
	return all, nil
}

// compareTime compares presentation times exactly.
func compareTime(a, b media.Packet) int {
	sa, errA := a.PTSSeconds()
	sb, errB := b.PTSSeconds()
	if errA != nil || errB != nil {
		// Fall back to raw PTS, which is only comparable within a base.
		switch {
		case a.PTS < b.PTS:
			return -1
		case a.PTS > b.PTS:
			return 1
		default:
			return 0
		}
	}
	return sa.Cmp(sb)
}

// Run drains a packet channel and returns the sorted output. It respects
// cancellation from ctx and from the source channel closing.
func (m *Merger) Run(ctx context.Context, in <-chan media.Packet) ([]media.Packet, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, media.New(media.KindCancel, media.Origin{Stage: "merge"}, "merge.Run", "canceled", ctx.Err())
		case pkt, ok := <-in:
			if !ok {
				return m.Sorted()
			}
			if _, err := m.Add(pkt); err != nil {
				return nil, err
			}
		}
	}
}

// Stats returns the merge statistics.
func (m *Merger) Stats() Stats { return m.stats }

// Quarantined returns the retained quarantined packets.
func (m *Merger) Quarantined() []Quarantined { return m.quarant }

// GroupByTrack reorganises an ordered packet list into per-track slices while
// preserving relative order. Remux and audit use this to keep each output
// stream's packets contiguous.
func GroupByTrack(pkts []media.Packet) map[string][]media.Packet {
	out := map[string][]media.Packet{}
	for _, p := range pkts {
		out[p.TrackID] = append(out[p.TrackID], p)
	}
	return out
}

// ValidateOrder verifies that an ordered packet list never moves backwards in
// presentation time within a track and never repeats a packet.
func ValidateOrder(pkts []media.Packet) error {
	seen := map[string]bool{}
	last := map[string]int64{}
	lastSet := map[string]bool{}
	for _, p := range pkts {
		if seen[p.Hash()] {
			return media.Errorf(media.KindPolicy, p.Origin, "merge.ValidateOrder",
				"duplicate packet %s", p.Fingerprint())
		}
		seen[p.Hash()] = true
		if !p.HasPTS() {
			return media.Errorf(media.KindCorrupt, p.Origin, "merge.ValidateOrder",
				"packet %s has no PTS", p.Fingerprint())
		}
		if lastSet[p.TrackID] && p.PTS < last[p.TrackID] {
			return media.Errorf(media.KindPolicy, p.Origin, "merge.ValidateOrder",
				"PTS regressed on %s: %d < %d", p.TrackID, p.PTS, last[p.TrackID])
		}
		last[p.TrackID] = p.PTS
		lastSet[p.TrackID] = true
	}
	return nil
}
