// Package transport turns the noisy packet stream from ingest into an
// ordered, de-duplicated timeline and recovers single-packet losses with
// block XOR forward error correction.
package transport

import (
	"sort"

	"avpipe/internal/media"
)

// DedupStats counts what the ordering stage observed.
type DedupStats struct {
	ExactDuplicates       int // identical retransmissions, silently removed
	ConflictingDuplicates int // same sequence number, different payload
	Wraparounds           int // 33-bit PTS wraparound crossings detected
	Reordered             int // packets delivered out of sequence
}

// ReorderDedup removes duplicates, resolves conflicts, detects PTS
// wraparound and returns packets ordered by unwrapped timestamp (sequence
// number breaks ties). Recoverable errors describe each anomaly.
// packetID uniquely identifies a transport packet. Media packets are
// distinguished by (kind, seq); parity packets additionally need ParityOf
// and BlockID because every block's parity shares KindParity and their seq
// encoding must not collide with media sequence numbers.
type packetID struct {
	kind     media.PacketKind
	parityOf media.PacketKind
	block    uint32
	seq      uint32
}

func identity(p *media.Packet) packetID {
	return packetID{kind: p.Kind, parityOf: p.ParityOf, block: p.BlockID, seq: p.Seq}
}

// streamKey groups a packet with the media stream it belongs to so video,
// audio and their parity packets never interleave during sorting.
func streamKey(p *media.Packet) uint32 {
	if p.Kind == media.KindParity {
		return uint32(p.ParityOf)
	}
	return uint32(p.Kind)
}

func ReorderDedup(in []*media.Packet) ([]*media.Packet, DedupStats, []*media.Error) {
	var stats DedupStats
	var errs []*media.Error
	seen := make(map[packetID]*media.Packet, len(in))
	out := make([]*media.Packet, 0, len(in))

	for _, p := range in {
		k := identity(p)
		if existing, ok := seen[k]; ok {
			if bytesEqual(existing.SHA256, p.SHA256) && bytesEqual(existing.Payload, p.Payload) {
				stats.ExactDuplicates++
				continue
			}
			stats.ConflictingDuplicates++
			errs = append(errs, media.NewError(media.ClassDuplicate, "transport",
				"conflicting duplicate for seq "+u32toa(p.Seq), p.PTS, true))
			continue // keep the first received copy, reject the conflict
		}
		seen[k] = p
		out = append(out, p)
	}

	// First pass over delivery order detects 33-bit wraparound per stream.
	prevByKind := map[media.PacketKind]uint64{}
	for _, p := range in {
		prev, ok := prevByKind[p.Kind]
		if !ok {
			prevByKind[p.Kind] = p.PTS % media.PTSModulo
			continue
		}
		unwrapped, wrapped := media.Unwrap(p.PTS, prev)
		if wrapped {
			stats.Wraparounds++
			errs = append(errs, media.NewError(media.ClassClock, "transport",
				"PTS wraparound detected near seq "+u32toa(p.Seq), unwrapped, true))
		}
		prevByKind[p.Kind] = unwrapped
	}

	// Reorder each independent stream by its transport sequence number.
	// Sorting by global PTS would interleave video, audio and parity
	// packets that happen to share a clock value and break FEC block
	// membership. Parity sorts after the media stream it protects.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		ak, bk := streamKey(a), streamKey(b)
		if ak != bk {
			return ak < bk
		}
		if a.Kind == media.KindParity && b.Kind == media.KindParity {
			return a.BlockID < b.BlockID
		}
		return a.Seq < b.Seq
	})
	// Count how many packets moved relative to their delivery position.
	rank := make(map[packetID]int, len(out))
	for i, p := range out {
		rank[identity(p)] = i
	}
	last := -1
	for _, p := range in {
		if r, ok := rank[identity(p)]; ok {
			if r < last {
				stats.Reordered++
			}
			if r > last {
				last = r
			}
		}
	}
	return out, stats, errs
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func u32toa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
