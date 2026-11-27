// Package fixture generates deterministic local HLS/DASH playlists, RTP
// packet streams, parity blocks and corruption patterns. Nothing is read
// from the network or an external file: a fixed seed fully determines the
// output, so repeated runs yield identical semantic summaries.
package fixture

import (
	"encoding/binary"
	"fmt"
	"math/rand"

	pionrtp "github.com/pion/rtp"

	"archive/internal/model"
	"archive/internal/rtp"
)

// Params controls one generated stream. The zero value is invalid: callers
// always pass the CLI-derived parameters.
type Params struct {
	Seed             int64
	Segments         int
	PacketsPerSegment int
	ClockRate        uint32
	TSIncrement      uint32 // RTP timestamp ticks between packets
	PayloadBodyBytes int
	StartSeq         uint16
	SSRC             uint32

	// Fault injection knobs.
	DuplicateEvery  int // >0: duplicate every Nth packet
	ReorderWindow   int // >0: shuffle within a sliding window of this size
	LossBurst       int // >0: drop this many packets per injected burst
	LossEvery       int // burst cadence in packets
	CorruptEvery    int // >0: flip a CRC byte every Nth surviving packet

	// Strict marks the generated case as requiring full recovery.
	Strict bool
}

// DefaultParams returns the archive demo defaults for one track.
func DefaultParams(seed int64) Params {
	return Params{
		Seed: seed, Segments: 4, PacketsPerSegment: 24, ClockRate: 90000,
		TSIncrement: 3000, PayloadBodyBytes: 32, StartSeq: 1000, SSRC: 0xC0FFEE,
		DuplicateEvery: 0, ReorderWindow: 0, LossBurst: 0, LossEvery: 0,
		CorruptEvery: 0,
	}
}

// Packet is one generated RTP datagram plus its generation metadata.
type Packet struct {
	Datagram []byte
	Seq      uint16
	TS       uint32
	ExtSeq   int64
	Payload  []byte // includes CRC trailer
	IsParity bool
	Group    []int64 // member ext-seqs for parity packets
	Segment  int
}

// Stream is the generated, fault-injected material for one track.
type Stream struct {
	Params  Params
	Packets []*Packet // arrival order, after reorder/loss/duplication
	// IdealPackets holds every packet that *should* exist, in sequence
	// order, including ones later lost. It is the recovery oracle.
	IdealPackets []*Packet
	Parity       []*Packet
	LostExtSeqs  []int64
	// RetransRecord models the sender's finite retransmission log. A lost
	// sequence appears here only when the sender still holds a copy; the
	// tail of a multi-packet burst aged out or was never logged, so those
	// holes have no retransmission source and must become explicit gaps.
	RetransRecord map[int64][]byte
	SegmentBound  []model.Nano // start time of each segment, shared clock
}

// payloadFor deterministically builds the CRC-protected body for ext-seq s.
// The body embeds the sequence and timestamp so recovered payloads can be
// byte-compared against the oracle.
func payloadFor(p Params, extSeq int64, ts uint32) []byte {
	body := make([]byte, p.PayloadBodyBytes)
	binary.BigEndian.PutUint64(body[0:8], uint64(extSeq))
	binary.BigEndian.PutUint32(body[8:12], ts)
	// Fill the remainder from a per-sequence deterministic source so content
	// varies but stays reproducible without sharing global RNG state.
	r := rand.New(rand.NewSource(p.Seed ^ extSeq*1_000_003))
	r.Read(body[12:])
	return append(body, rtp.MakeCRCTrailer(body)...)
}

func marshalDatagram(p Params, seq uint16, ts uint32, payload []byte) []byte {
	pkt := pionrtp.Packet{
		Header: pionrtp.Header{
			Version: 2, PayloadType: 96, SequenceNumber: seq,
			Timestamp: ts, SSRC: p.SSRC,
		},
		Payload: payload,
	}
	buf, err := pkt.Marshal()
	if err != nil {
		panic(fmt.Sprintf("fixture: marshal failed: %v", err))
	}
	return buf
}

// Generate builds one track: ideal packets first, then parity groups, then
// the fault-injected arrival stream. All randomness comes from a rand
// source seeded solely by Params.Seed.
func Generate(p Params) *Stream {
	if p.PacketsPerSegment <= 0 || p.PayloadBodyBytes <= 0 {
		panic("fixture: invalid generation params")
	}
	rng := rand.New(rand.NewSource(p.Seed))
	ext := model.NewSeqExt()
	total := p.Segments * p.PacketsPerSegment

	ideal := make([]*Packet, 0, total)
	bounds := make([]model.Nano, 0, p.Segments+1)
	ts := uint32(0)
	tsToNano := func(t uint32) model.Nano {
		return model.Nano(int64(t) * model.Second.Nanos() / int64(p.ClockRate))
	}
	for i := 0; i < total; i++ {
		seq := uint16((int(p.StartSeq) + i) % 65536)
		extSeq := ext.Extend(seq)
		payload := payloadFor(p, extSeq, ts)
		ideal = append(ideal, &Packet{
			Datagram: marshalDatagram(p, seq, ts, payload),
			Seq: seq, TS: ts, ExtSeq: extSeq, Payload: payload,
			Segment: i / p.PacketsPerSegment,
		})
		if i%p.PacketsPerSegment == 0 {
			bounds = append(bounds, tsToNano(ts))
		}
		ts += p.TSIncrement
	}
	bounds = append(bounds, tsToNano(ts))

	st := &Stream{Params: p, IdealPackets: ideal, SegmentBound: bounds}
	st.Parity, st.LostExtSeqs = st.inject(rng)
	st.Packets = st.buildArrival(rng)
	return st
}

// IdealPayload returns the oracle payload for an ext-sequence, or nil.
func (s *Stream) IdealPayload(extSeq int64) []byte {
	for _, pk := range s.IdealPackets {
		if pk.ExtSeq == extSeq {
			return pk.Payload
		}
	}
	return nil
}
