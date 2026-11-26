// Package transport simulates the numbered packet stream delivered from
// supplier capture: it replays deterministic loss, duplicates and tail
// truncation, then recovers missing media packets with XOR parity groups.
// Every outcome keeps the declared source offset and raw PTS.
package transport

import (
	"fmt"

	"bq/internal/manifest"
	"bq/internal/model"
	"bq/internal/randx"
)

// Stats summarises one stream's recovery result.
type Stats struct {
	StreamID             int   `json:"stream_id"`
	DeclaredMedia        int64 `json:"declared_media"`
	ReceivedMedia        int64 `json:"received_media"`
	RecoveredPackets     int64 `json:"recovered_packets"`
	UnrecoveredPackets   int64 `json:"unrecovered_packets"`
	DuplicatePackets     int64 `json:"duplicate_packets"`
	TruncatedPackets     int64 `json:"truncated_packets"`
	ParityGroups         int64 `json:"parity_groups"`
	ParityGroupsRecovered int64 `json:"parity_groups_recovered"`
}

// Result bundles the recovered stream packets and per-stream statistics.
type Result struct {
	Packets []manifest.Packet
	Stats   map[int]*Stats
	Errors  []model.Error
}

// payloadByte produces the deterministic content byte of a media packet so
// test fixtures and parity generation stay reproducible without storing
// payloads. It depends only on stream id, group id and sequence.
func payloadByte(p manifest.Packet) byte {
	h := uint64(p.StreamID)*1_000_003 + uint64(p.GroupID)*9_173 + uint64(p.Seq)*131 + 7
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	return byte(h)
}

// parityByte returns the XOR check byte for a group: the XOR of every media
// packet's content byte. Fixture generators assign this to the parity packet.
func parityByte(media []manifest.Packet) byte {
	var xor byte
	for _, p := range media {
		if p.Kind == manifest.KindMedia {
			xor ^= payloadByte(p)
		}
	}
	return xor
}

// SimulateDelivery applies the manifest's deterministic injection plan to the
// declared packets and returns what actually arrived at the pipeline.
func SimulateDelivery(m *manifest.Manifest) []manifest.Packet {
	rng := randx.NewPCG64(m.Seed ^ 0x5eed)
	dropped := dropSet(m, rng)
	dupSet := make(map[[2]int64]bool)
	for _, d := range m.Injection.Duplicates {
		dupSet[[2]int64{int64(d.StreamID), d.Seq}] = true
	}
	trunc := make(map[int]int64)
	for _, tr := range m.Injection.InjectionTruncation() {
		trunc[tr.StreamID] = tr.AfterSeq
	}

	var delivered []manifest.Packet
	for _, p := range m.Packets {
		if after, ok := trunc[p.StreamID]; ok && p.Kind == manifest.KindMedia && p.Seq > after {
			continue // captured tail missing
		}
		key := [2]int64{int64(p.StreamID), p.Seq}
		// Loss is injected only into media packets; parity packets must
		// survive so single-loss groups remain reconstructable.
		if p.Kind == manifest.KindMedia && dropped[key] {
			continue
		}
		delivered = append(delivered, p)
		if p.Kind == manifest.KindMedia && dupSet[key] {
			delivered = append(delivered, p) // exact re-delivery
		}
	}
	return delivered
}

func dropSet(m *manifest.Manifest, rng *randx.PCG64) map[[2]int64]bool {
	out := make(map[[2]int64]bool)
	if m.Injection.LossRate <= 0 {
		return out
	}
	for _, p := range m.Packets {
		if p.Kind != manifest.KindMedia {
			continue
		}
		if rng.Float64() < m.Injection.LossRate {
			out[[2]int64{int64(p.StreamID), p.Seq}] = true
		}
	}
	return out
}

// Recover consumes a delivered stream, removes duplicates, reconstructs
// single-loss groups via XOR parity, and accounts for unrecoverable gaps.
func Recover(m *manifest.Manifest, delivered []manifest.Packet) *Result {
	res := &Result{Stats: make(map[int]*Stats)}
	declared := manifest.MediaPackets(m.Packets)
	for _, p := range declared {
		st := res.stats(p.StreamID)
		st.DeclaredMedia++
	}

	seen := make(map[[2]int64]bool)
	groups := make(map[int]map[int64][]manifest.Packet) // stream -> group -> packets
	for _, p := range delivered {
		st := res.stats(p.StreamID)
		if p.Kind == manifest.KindParity {
			st.ParityGroups++
		}
		key := [2]int64{int64(p.StreamID), p.Seq}
		if p.Kind == manifest.KindMedia {
			if seen[key] {
				st.DuplicatePackets++
				res.Errors = append(res.Errors, model.NewError(model.StageTransport, p.StreamID,
					model.ErrDuplicatePacket, model.SeverityWarning,
					fmt.Sprintf("duplicate seq=%d dropped", p.Seq), p.SourceOffset, p.RawPTS, true))
				continue
			}
			seen[key] = true
			st.ReceivedMedia++
		}
		res.Packets = append(res.Packets, p)
		if groups[p.StreamID] == nil {
			groups[p.StreamID] = make(map[int64][]manifest.Packet)
		}
		groups[p.StreamID][p.GroupID] = append(groups[p.StreamID][p.GroupID], p)
	}

	res.applyTruncation(m)
	res.recoverGroups(m, groups, seen)
	return res
}

func (r *Result) stats(streamID int) *Stats {
	st, ok := r.Stats[streamID]
	if !ok {
		st = &Stats{StreamID: streamID}
		r.Stats[streamID] = st
	}
	return st
}

// recoverGroups reconstructs groups missing exactly one media packet.
func (r *Result) recoverGroups(m *manifest.Manifest,
	groups map[int]map[int64][]manifest.Packet, seen map[[2]int64]bool) {

	groupIndex := manifest.GroupPackets(m.Packets)
	for streamID, gs := range groups {
		st := r.Stats[streamID]
		for gid, arrived := range gs {
			declared := groupIndex[gid]
			hasParity := false
			for _, p := range arrived {
				if p.Kind == manifest.KindParity {
					hasParity = true
				}
			}
			if !hasParity {
				continue // parity itself lost: nothing to reconstruct from
			}
			var missing []manifest.Packet
			for _, dp := range declared {
				if dp.Kind != manifest.KindMedia {
					continue
				}
				if !seen[[2]int64{int64(dp.StreamID), dp.Seq}] {
					missing = append(missing, dp)
				}
			}
			if len(missing) == 0 {
				continue
			}
			if len(missing) > 1 {
				for _, miss := range missing {
					r.recordUnrecovered(miss, "group lost >1 media packet, XOR cannot reconstruct")
				}
				continue
			}
			miss := missing[0]
			if verifyParity(miss, arrived) {
				r.Packets = append(r.Packets, miss)
				seen[[2]int64{int64(miss.StreamID), miss.Seq}] = true
				st.RecoveredPackets++
				st.ParityGroupsRecovered++
				r.Errors = append(r.Errors, model.NewError(model.StageTransport, miss.StreamID,
					model.ErrPacketLoss, model.SeverityRecovered,
					fmt.Sprintf("seq=%d reconstructed from parity group %d", miss.Seq, gid),
					miss.SourceOffset, miss.RawPTS, true))
			} else {
				r.recordUnrecovered(miss, "reconstructed parity mismatch")
			}
		}
	}
	r.accountMissing(m)
}

// verifyParity checks the XOR identity with the explicit check bytes carried
// in the manifest: XOR of all media content bytes (arrived plus the candidate
// missing packet) must equal the delivered parity packet's check byte.
func verifyParity(missing manifest.Packet, arrived []manifest.Packet) bool {
	var mediaXOR byte
	var parity byte
	haveParity := false
	for _, p := range arrived {
		if p.Kind == manifest.KindParity {
			parity = p.XOR
			haveParity = true
			continue
		}
		mediaXOR ^= p.XOR
	}
	missingByte := missing.XOR
	if missingByte == 0 {
		missingByte = payloadByte(missing)
	}
	return haveParity && mediaXOR^missingByte == parity
}

func (r *Result) recordUnrecovered(p manifest.Packet, msg string) {
	st := r.Stats[p.StreamID]
	st.UnrecoveredPackets++
	r.Errors = append(r.Errors, model.NewError(model.StageTransport, p.StreamID,
		model.ErrPacketUnrecovered, model.SeverityError, msg+
			fmt.Sprintf(" seq=%d", p.Seq), p.SourceOffset, p.RawPTS, true))
}

// accountMissing flags declared media packets absent for any reason other
// than truncation and not already reconstructed.
func (r *Result) accountMissing(m *manifest.Manifest) {
	present := make(map[[2]int64]bool)
	for _, p := range r.Packets {
		if p.Kind == manifest.KindMedia {
			present[[2]int64{int64(p.StreamID), p.Seq}] = true
		}
	}
	for _, p := range manifest.MediaPackets(m.Packets) {
		if present[[2]int64{int64(p.StreamID), p.Seq}] {
			continue
		}
		// Tail-truncated packets are classified by applyTruncation.
		if after := m.TruncAfter(p.StreamID); after >= 0 && p.Seq > after {
			continue
		}
		// Only count once: recoverGroups already counted parity failures.
		if alreadyCounted(r.Errors, p) {
			continue
		}
		r.recordUnrecovered(p, "media packet absent and not reconstructable")
	}
}

func alreadyCounted(errs []model.Error, p manifest.Packet) bool {
	for _, e := range errs {
		if e.StreamID == p.StreamID && e.HasPTS && e.OriginalPTS == p.RawPTS &&
			e.SourceOffset == p.SourceOffset {
			return true
		}
	}
	return false
}

func (r *Result) applyTruncation(m *manifest.Manifest) {
	for _, tr := range m.Injection.InjectionTruncation() {
		st := r.Stats[tr.StreamID]
		var cut int64
		for _, p := range manifest.MediaPackets(m.Packets) {
			if p.StreamID == tr.StreamID && p.Seq > tr.AfterSeq {
				cut++
			}
		}
		if cut == 0 {
			continue
		}
		st.TruncatedPackets = cut
		last := tr.AfterSeq
		r.Errors = append(r.Errors, model.NewError(model.StageTransport, tr.StreamID,
			model.ErrTailTruncation, model.SeverityError,
			fmt.Sprintf("tail truncated after seq=%d, %d declared packets missing", last, cut),
			-1, 0, false))
	}
}
