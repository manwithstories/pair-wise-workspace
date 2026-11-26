package fixture

import (
	"sort"
)

// ApplyDamage returns a new Clip with the damage spec applied. The source
// clip is left untouched so clean and damaged runs share identical inputs.
func ApplyDamage(src *Clip) *Clip {
	out := &Clip{
		Seed:    src.Seed,
		Tracks:  src.Tracks,
		Spec:    src.Spec,
		Packets: append([]Packet(nil), src.Packets...),
		Events:  append([]Event(nil), src.Events...),
	}
	applyDrops(out)
	applyDuplicates(out)
	out.Packets = applyReorder(out.Seed, out.Packets, out.Spec.ReorderWindow)
	applyTruncation(out)
	return out
}

// applyDrops marks media packets in targeted FEC groups as dropped. Single
// losses stay recoverable; multiple losses create precise, unrecovered gaps.
func applyDrops(clip *Clip) {
	dropIdx := map[int]bool{}
	for _, g := range clip.Spec.DropSingles {
		if i := firstMediaInGroup(clip, g); i >= 0 {
			dropIdx[i] = true
		}
	}
	for g, n := range clip.Spec.DropMultiples {
		idxs := mediaInGroup(clip, g)
		if n > len(idxs) {
			n = len(idxs)
		}
		for k := 0; k < n; k++ {
			dropIdx[idxs[k]] = true
		}
	}
	for i := range clip.Packets {
		if dropIdx[i] {
			clip.Packets[i].Dropped = true
		}
	}
}

func firstMediaInGroup(clip *Clip, group int) int {
	for i, p := range clip.Packets {
		if !p.IsParity && p.FECGroup == group && !p.Dropped {
			return i
		}
	}
	return -1
}

func mediaInGroup(clip *Clip, group int) []int {
	var idxs []int
	for i, p := range clip.Packets {
		if !p.IsParity && p.FECGroup == group {
			idxs = append(idxs, i)
		}
	}
	return idxs
}

// applyDuplicates flags configured sequence numbers; the transport layer is
// expected to collapse them back to a single packet.
func applyDuplicates(clip *Clip) {
	if len(clip.Spec.DuplicateSeq) == 0 {
		return
	}
	for i := range clip.Packets {
		p := &clip.Packets[i]
		for _, seq := range clip.Spec.DuplicateSeq[p.Track] {
			if p.Seq == seq && !p.IsParity {
				p.Duplicated = true
			}
		}
	}
}

// applyReorder shuffles delivery positions inside a bounded sliding window.
// Dropped packets are removed first so the stream models real delivery.
func applyReorder(seed int64, pkts []Packet, window int) []Packet {
	var live []Packet
	for _, p := range pkts {
		if !p.Dropped {
			live = append(live, p)
		}
	}
	if window <= 1 {
		return live
	}
	r := newRand(seed, 4242)
	for start := 0; start < len(live); start += window {
		end := start + window
		if end > len(live) {
			end = len(live)
		}
		block := live[start:end]
		// Deterministic Fisher-Yates within the window.
		for i := len(block) - 1; i > 0; i-- {
			j := r.IntN(i + 1)
			block[i], block[j] = block[j], block[i]
		}
	}
	return live
}

// applyTruncation cuts flagged event payloads to a fragment, preserving the
// first-offset information the metadata layer must report.
func applyTruncation(clip *Clip) {
	for _, idx := range clip.Spec.TruncateEvents {
		if idx < 0 || idx >= len(clip.Events) {
			continue
		}
		ev := &clip.Events[idx]
		if len(ev.Payload) > 4 {
			ev.Truncated = true
			ev.Payload = append([]byte(nil), ev.Payload[:len(ev.Payload)/3]...)
		}
	}
}

// ExpandDuplicates returns a delivery-order slice in which flagged packets
// appear twice, emulating duplicated RTP arrival before dedup.
func ExpandDuplicates(pkts []Packet) []Packet {
	var out []Packet
	for _, p := range pkts {
		out = append(out, p)
		if p.Duplicated {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Track != out[j].Track {
			return out[i].Track < out[j].Track
		}
		return out[i].Seq < out[j].Seq
	})
	return out
}
