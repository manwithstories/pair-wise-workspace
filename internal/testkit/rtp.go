package testkit

import (
	"fmt"
	"math/rand"
	"sort"

	"github.com/pion/rtp"
)

// RTPFixture is a deterministic, self-generated packet-loss scenario. Packets
// carry the same payload framing as the archive ingest path: every access unit
// is split into fragments, each fragment is one RTP packet, and the marker bit
// is set on the final fragment.
type RTPFixture struct {
	Seed        int64
	SSRC        uint32
	PayloadType uint8
	Timestamp   uint32
	// Sent is the number of packets the sender put on the wire.
	Sent uint64
	// PayloadBytes is the total payload carried by sent packets.
	PayloadBytes uint64
	// Wire holds the packets actually delivered to the receiver, after loss,
	// duplication and reordering.
	Wire []*rtp.Packet
	// Origin maps a sequence number to its send index for ground truth.
	Origin map[uint16]uint64
}

// GenerateRTP builds a deterministic loss/reorder/duplication fixture.
//
// loss is the probability that a packet is dropped, in [0,1). Roughly 2% of the
// surviving packets are duplicated and the wire order is perturbed by a bounded
// shuffle window so that reordering is guaranteed at the configured scale.
func GenerateRTP(seed int64, loss float64, packetCount int) (*RTPFixture, error) {
	if loss < 0 || loss >= 1 {
		return nil, fmt.Errorf("loss %.3f out of range [0,1)", loss)
	}
	if packetCount <= 0 || packetCount > 200_000 {
		return nil, fmt.Errorf("packet count %d out of range (1..200000)", packetCount)
	}
	rng := rand.New(rand.NewSource(seed))
	f := &RTPFixture{
		Seed:        seed,
		SSRC:        0x5eed0000 | uint32(seed&0xffff),
		PayloadType: 96,
		Timestamp:   90000,
		Origin:      map[uint16]uint64{},
	}
	const fragSize = 512
	for i := 0; i < packetCount; i++ {
		// Payload bytes vary deterministically to exercise length handling.
		n := fragSize - (i*37)%97
		payload := make([]byte, n)
		for j := range payload {
			payload[j] = byte((i*131 + j*17) & 0xff)
		}
		seq := uint16(i % 65536)
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				SequenceNumber: seq,
				Timestamp:      f.Timestamp,
				SSRC:           f.SSRC,
				PayloadType:    f.PayloadType,
				Marker:         i%20 == 19,
			},
			Payload: payload,
		}
		f.Sent++
		f.PayloadBytes += uint64(n)
		f.Origin[seq] = uint64(i)
		// The wire keeps a copy even when it is later dropped; dropping is a
		// separate decision so the fixture stays auditable.
		if rng.Float64() < loss {
			continue
		}
		f.Wire = append(f.Wire, pkt)
		// Duplicate a small, deterministic fraction.
		if rng.Float64() < 0.02 {
			f.Wire = append(f.Wire, pkt.Clone())
		}
	}
	// Bounded shuffle to produce genuine out-of-order arrival.
	for i := len(f.Wire) - 1; i > 0; i-- {
		j := i - rng.Intn(minInt(i, 8)+1)
		f.Wire[i], f.Wire[j] = f.Wire[j], f.Wire[i]
	}
	return f, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// RecoveryStats is the machine-readable outcome of RTP reassembly.
type RecoveryStats struct {
	Received      int     `json:"received"`
	Duplicates    int     `json:"duplicates"`
	Reordered     int     `json:"reordered"`
	UniquePackets int     `json:"unique_packets"`
	Recovered     int     `json:"recovered_payload_bytes"`
	Total         int     `json:"total_payload_bytes"`
	RecoveryRate  float64 `json:"loss_recovery_rate"`
	// Gaps counts sequence discontinuities. RepairedGaps counts gaps that were
	// filled by a later duplicate (the only repair available without
	// retransmission in a local archive ingest).
	Gaps         int `json:"gaps"`
	RepairedGaps int `json:"repaired_gaps"`
	LostPackets  int `json:"lost_packets"`
}

// RecoverRTP reassembles the fixture wire order. It sorts by sequence number,
// removes duplicates, counts reordering against arrival order, and reports
// payload coverage relative to what the sender emitted.
func RecoverRTP(f *RTPFixture) *RecoveryStats {
	st := &RecoveryStats{
		Received: len(f.Wire),
		Total:    int(f.PayloadBytes),
	}
	type view struct {
		pkt   *rtp.Packet
		first int
	}
	uniq := map[uint16]*view{}
	for i, p := range f.Wire {
		if v, ok := uniq[p.SequenceNumber]; ok {
			st.Duplicates++
			// A duplicate is useful when the first copy was not the same length
			// (truncated on the wire); keep the larger payload.
			if len(p.Payload) > len(v.pkt.Payload) {
				v.pkt = p
			}
			continue
		}
		uniq[p.SequenceNumber] = &view{pkt: p, first: i}
	}
	seqs := make([]int, 0, len(uniq))
	for s := range uniq {
		seqs = append(seqs, int(s))
	}
	sort.Ints(seqs)
	st.UniquePackets = len(seqs)
	// Reordering: a packet whose arrival index is earlier than the previous
	// sequence number's arrival index.
	prevArrival := -1
	for _, s := range seqs {
		v := uniq[uint16(s)]
		if v.first < prevArrival {
			st.Reordered++
		}
		prevArrival = v.first
	}
	for i, s := range seqs {
		st.Recovered += len(uniq[uint16(s)].pkt.Payload)
		if i == 0 {
			continue
		}
		if s != seqs[i-1]+1 {
			st.Gaps++
			st.LostPackets += s - seqs[i-1] - 1
		}
	}
	if st.Total > 0 {
		st.RecoveryRate = float64(st.Recovered) / float64(st.Total)
	}
	return st
}
