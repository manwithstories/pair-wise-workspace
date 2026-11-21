package fixture

import (
	pionrtp "github.com/pion/rtp"
)

// RTPPacket is one packet plus the sample it belongs to.
type RTPPacket struct {
	SampleIndex int
	Packet      *pionrtp.Packet
}

// Packetise turns a deterministic payload stream into RTP packets: one packet
// per sample, timestamps in the media clock, and a marker on every frame.
// Sequence numbers start at startSeq so wrap behaviour can be exercised.
func Packetise(seed int64, trackID uint32, samples, payloadBytes, clockRate, ticksPerFrame int, startSeq uint16, startTS uint32) []RTPPacket {
	if ticksPerFrame <= 0 {
		ticksPerFrame = clockRate / 30
	}
	out := make([]RTPPacket, 0, samples)
	buf := make([]byte, payloadBytes)
	for i := 0; i < samples; i++ {
		buf = Payload(seed, trackID, i, payloadBytes, buf)
		ts := startTS + uint32(int64(i)*int64(ticksPerFrame))
		out = append(out, RTPPacket{
			SampleIndex: i,
			Packet: &pionrtp.Packet{
				Header: pionrtp.Header{
					Version:        2,
					PayloadType:    96,
					SequenceNumber: startSeq + uint16(i),
					Timestamp:      ts,
					Marker:         true,
				},
				Payload: append([]byte(nil), buf...),
			},
		})
	}
	return out
}

// FaultKind names an injected network fault.
type FaultKind string

const (
	// FaultDuplicate repeats a packet.
	FaultDuplicate FaultKind = "duplicate"
	// FaultReorder swaps a packet behind its successor.
	FaultReorder FaultKind = "reorder"
	// FaultBurstLoss drops a contiguous run of packets.
	FaultBurstLoss FaultKind = "burst-loss"
	// FaultCorruptLog damages a committed artifact so recovery must roll
	// back. It is handled by the pipeline, not by ApplyFaults.
	FaultCorruptLog FaultKind = "corrupt-log"
)

// FaultParams tunes the injected faults. Zero values disable a fault.
type FaultParams struct {
	// DuplicateAt lists packet indices to send twice.
	DuplicateAt []int
	// ReorderAt lists packet indices to delay behind the next packet.
	ReorderAt []int
	// BurstStart and BurstLen describe the first lost run. A short run is
	// recoverable from intact neighbours.
	BurstStart int
	BurstLen   int
	// Burst2Start and Burst2Len describe a second, deliberately longer run
	// that must be reported as unrecoverable.
	Burst2Start int
	Burst2Len   int
}

// ApplyFaults returns the packets to feed the receiver, in arrival order,
// along with the exact count of injected faults. Duplicates are re-sent
// immediately, a reordered packet is emitted after its successor, and the
// burst is removed entirely.
func ApplyFaults(pkts []RTPPacket, kinds []FaultKind, p FaultParams) ([]*pionrtp.Packet, int) {
	want := map[FaultKind]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	dupAt := map[int]bool{}
	for _, i := range p.DuplicateAt {
		dupAt[i] = true
	}
	reorderAt := map[int]bool{}
	for _, i := range p.ReorderAt {
		reorderAt[i] = true
	}
	burstEnd, burst2End := -1, -1
	if want[FaultBurstLoss] {
		if p.BurstLen > 0 {
			burstEnd = p.BurstStart + p.BurstLen
		}
		if p.Burst2Len > 0 {
			burst2End = p.Burst2Start + p.Burst2Len
		}
	}

	var out []*pionrtp.Packet
	faults := 0
	for i := 0; i < len(pkts); i++ {
		if (burstEnd > 0 && i >= p.BurstStart && i < burstEnd) ||
			(burst2End > 0 && i >= p.Burst2Start && i < burst2End) {
			faults++
			continue
		}
		pkt := pkts[i].Packet
		if want[FaultReorder] && reorderAt[i] && i+1 < len(pkts) {
			// Emit the successor first, then the held packet.
			out = append(out, pkts[i+1].Packet)
			if want[FaultDuplicate] && dupAt[i] {
				out = append(out, clone(pkt))
				faults++
			}
			out = append(out, pkt)
			faults++
			i++ // the successor has been emitted
			continue
		}
		out = append(out, pkt)
		if want[FaultDuplicate] && dupAt[i] {
			out = append(out, clone(pkt))
			faults++
		}
	}
	return out, faults
}

func clone(p *pionrtp.Packet) *pionrtp.Packet {
	c := p.Clone()
	return c
}

// SampleBytes returns the deterministic bytes for a sample.
func SampleBytes(seed int64, trackID uint32, sampleIndex, size int) []byte {
	return Payload(seed, trackID, sampleIndex, size, nil)
}
