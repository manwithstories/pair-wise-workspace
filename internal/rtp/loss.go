// Package rtp builds local, deterministic RTP loss/reorder/duplicate
// samples with pion/rtp and reassembles them over a bounded sequence-number
// window. Unrecoverable gaps reuse the shared model error vocabulary so RTP
// statistics and manifest gaps speak the same code language.
package rtp

import (
	"github.com/pion/rtp"
)

// Packet is a locally generated RTP packet carrying one synthetic payload.
type Packet struct {
	Seq       uint16
	Timestamp uint32
	SSRC      uint32
	Payload   []byte
	Marker    bool
}

// LossConfig drives deterministic damage on an otherwise clean packet run.
type LossConfig struct {
	// BurstAt is the sequence number where a burst loss starts.
	BurstAt uint16
	// BurstLen is how many consecutive media packets are dropped.
	BurstLen int
	// Reorder swaps delivery of seq and seq+1 when nonzero.
	ReorderAt uint16
	// DuplicateAt delivers one packet twice when nonzero.
	DuplicateAt uint16
}

// Stream generates a clean run of packets, then applies local damage.
type Stream struct {
	ssrc       uint32
	clockRate  uint32
	packetStep uint32 // timestamp increment per packet
}

// NewStream configures a synthetic RTP source.
func NewStream(ssrc uint32, clockRate, packetStep uint32) *Stream {
	return &Stream{ssrc: ssrc, clockRate: clockRate, packetStep: packetStep}
}

// BuildPackets creates count packets with deterministic payloads. Payload
// embeds the sequence number so reassembly correctness is independently
// checkable, not inferred from arrival order.
func (s *Stream) BuildPackets(count int) []Packet {
	out := make([]Packet, count)
	for i := 0; i < count; i++ {
		seq := uint16(i)
		out[i] = Packet{
			Seq:       seq,
			Timestamp: uint32(i) * s.packetStep,
			SSRC:      s.ssrc,
			Payload:   syntheticPayload(seq),
			Marker:    i == count-1,
		}
	}
	return out
}

// ApplyDamage returns the delivered packet sequence after loss, reorder
// and duplication. It never invents packets; dropped seqs simply vanish.
func ApplyDamage(pkts []Packet, cfg LossConfig) []Packet {
	dropped := map[uint16]struct{}{}
	for i := 0; i < cfg.BurstLen; i++ {
		dropped[cfg.BurstAt+uint16(i)] = struct{}{}
	}
	var delivered []Packet
	for _, p := range pkts {
		if _, bad := dropped[p.Seq]; bad {
			continue
		}
		delivered = append(delivered, p)
	}
	if cfg.ReorderAt != 0 {
		swapAdjacent(delivered, cfg.ReorderAt)
	}
	if cfg.DuplicateAt != 0 {
		delivered = duplicate(delivered, cfg.DuplicateAt)
	}
	return delivered
}

// Marshal encodes packets through pion/rtp so the receiver exercises the
// real wire-format parser rather than an in-memory shortcut.
func Marshal(pkts []Packet) ([][]byte, error) {
	out := make([][]byte, 0, len(pkts))
	for _, p := range pkts {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: 96, SequenceNumber: p.Seq,
				Timestamp: p.Timestamp, SSRC: p.SSRC, Marker: p.Marker,
			},
			Payload: p.Payload,
		}
		buf := make([]byte, pkt.MarshalSize())
		if _, err := pkt.MarshalTo(buf); err != nil {
			return nil, err
		}
		out = append(out, buf)
	}
	return out, nil
}

// Unmarshal parses wire bytes back into pion packets.
func Unmarshal(wire [][]byte) ([]*rtp.Packet, error) {
	out := make([]*rtp.Packet, 0, len(wire))
	for _, b := range wire {
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(b); err != nil {
			return nil, err
		}
		out = append(out, pkt)
	}
	return out, nil
}

func syntheticPayload(seq uint16) []byte {
	// 16-byte payload: 2-byte seq echo plus deterministic fill.
	buf := make([]byte, 16)
	buf[0] = byte(seq >> 8)
	buf[1] = byte(seq)
	for i := 2; i < len(buf); i++ {
		buf[i] = byte(uint32(seq)*31 + uint32(i)*7)
	}
	return buf
}

func swapAdjacent(pkts []Packet, seq uint16) {
	for i := 0; i+1 < len(pkts); i++ {
		if pkts[i].Seq == seq && pkts[i+1].Seq == seq+1 {
			pkts[i], pkts[i+1] = pkts[i+1], pkts[i]
			return
		}
	}
}

func duplicate(pkts []Packet, seq uint16) []Packet {
	for i := range pkts {
		if pkts[i].Seq == seq {
			dup := pkts[i]
			return append(pkts[:i+1], append([]Packet{dup}, pkts[i+1:]...)...)
		}
	}
	return pkts
}

func relSeq(seq, base uint16) int {
	return int(int16(seq - base))
}
