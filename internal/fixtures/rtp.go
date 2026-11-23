package fixtures

import (
	"encoding/binary"
	"fmt"

	"github.com/pion/rtp"
)

// RTPStream is a generated RTP input: packets in wire order plus the original
// sample identities so recovery can be checked against ground truth.
type RTPStream struct {
	Name string
	// Packets are marshalled RTP packets in transmission order, including any
	// duplicates and reordering.
	Packets [][]byte
	// Recovered is the sample sequence the recovery step must reconstruct:
	// payloads in sequence order with gaps explicit.
	ExpectedSamples [][]byte
	// LostSequences lists sequence numbers that were never transmitted.
	LostSequences []uint16
	// DuplicateSequences lists sequence numbers sent more than once.
	DuplicateSequences []uint16
	// SSRC shared by every packet.
	SSRC uint32
	// ClockRate of the RTP timestamp.
	ClockRate uint32
	// PayloadType carried by every packet.
	PayloadType uint8
	// StartSeq is the first sequence number.
	StartSeq uint16
	// Timestamps per sample, one entry per sequence in order.
	SampleTimestamps []uint32
}

// RTPOptions controls the generated stream.
type RTPOptions struct {
	Seed        int64
	SSRC        uint32
	PayloadType uint8
	// ClockRate converts sample index to RTP timestamp.
	ClockRate uint32
	// Samples is the number of distinct sample payloads.
	Samples int
	// Duplicate sends this many chosen packets twice.
	Duplicate int
	// Reorder moves packets this many positions later (bounded).
	Reorder int
	// LossBurst drops a contiguous run of this many packets in the middle.
	LossBurst int
	// LossAt is the sequence index (0-based) where the loss burst starts.
	LossAt int
	// StartSeq overrides the first sequence number.
	StartSeq uint16
}

// DefaultRTPOptions returns a stream with no impairments.
func DefaultRTPOptions() RTPOptions {
	return RTPOptions{
		Seed: 41, SSRC: 0x5eed0001, PayloadType: 96, ClockRate: 90000,
		Samples: 60, StartSeq: 100,
	}
}

func (o RTPOptions) validated() (RTPOptions, error) {
	if o.Samples < 0 {
		return o, fmt.Errorf("fixtures: negative RTP sample count")
	}
	if o.ClockRate == 0 {
		return o, fmt.Errorf("fixtures: RTP clock rate must be positive")
	}
	if o.Duplicate < 0 || o.Reorder < 0 || o.LossBurst < 0 {
		return o, fmt.Errorf("fixtures: negative RTP impairment")
	}
	if o.LossBurst > o.Samples {
		o.LossBurst = o.Samples
	}
	// LossAt of zero with a non-zero burst means "unset": place the burst in
	// the middle so it is observable as a gap. Set LossAt explicitly to test
	// head-of-stream loss, which recovery cannot distinguish from a stream
	// that simply began later.
	if o.LossAt <= 0 && o.LossBurst > 0 {
		o.LossAt = o.Samples / 2
	}
	if o.LossAt < 0 || o.LossAt > o.Samples {
		o.LossAt = o.Samples / 2
	}
	return o, nil
}

// BuildRTP generates the stream and applies impairments deterministically.
func BuildRTP(o RTPOptions) (*RTPStream, error) {
	o, err := o.validated()
	if err != nil {
		return nil, err
	}
	st := &RTPStream{Name: "rtp-input.bin", SSRC: o.SSRC, ClockRate: o.ClockRate,
		PayloadType: o.PayloadType, StartSeq: o.StartSeq}

	// One coded sample per RTP packet keeps depacketisation unambiguous.
	payloads := make([][]byte, o.Samples)
	seqOf := make([]uint16, o.Samples)
	for i := 0; i < o.Samples; i++ {
		size := sampleSize(o.Seed, 0x2000, i, 260, 120)
		payloads[i] = Payload(o.Seed, 0x2000, i, size)
		seqOf[i] = o.StartSeq + uint16(i)
		st.SampleTimestamps = append(st.SampleTimestamps, uint32(i)*3000)
	}
	// Loss burst.
	lost := make(map[int]bool)
	start := o.LossAt
	end := start + o.LossBurst
	if end > o.Samples {
		end = o.Samples
	}
	for i := start; i < end; i++ {
		lost[i] = true
	}
	var kept []int
	for i := 0; i < o.Samples; i++ {
		if !lost[i] {
			kept = append(kept, i)
		} else {
			st.LostSequences = append(st.LostSequences, seqOf[i])
		}
	}
	for _, i := range kept {
		st.ExpectedSamples = append(st.ExpectedSamples, payloads[i])
	}

	// Build packets in transmission order.
	type pkt struct {
		seq  uint16
		idx  int
		data []byte
	}
	var pkts []pkt
	for _, i := range kept {
		raw, err := marshalRTP(o, seqOf[i], uint32(i)*3000, payloads[i], true)
		if err != nil {
			return nil, err
		}
		pkts = append(pkts, pkt{seq: seqOf[i], idx: i, data: raw})
	}
	// Duplicates: repeat evenly spaced packets, inserted right after originals.
	if o.Duplicate > 0 && len(pkts) > 0 {
		step := len(pkts) / o.Duplicate
		if step == 0 {
			step = 1
		}
		var withDup []pkt
		dups := 0
		for i, p := range pkts {
			withDup = append(withDup, p)
			if dups < o.Duplicate && i%step == 0 {
				cp := p
				cp.data = append([]byte(nil), p.data...)
				withDup = append(withDup, cp)
				st.DuplicateSequences = append(st.DuplicateSequences, p.seq)
				dups++
			}
		}
		pkts = withDup
	}
	// Reorder: swap each affected packet with one several positions later.
	if o.Reorder > 0 && len(pkts) > 1 {
		span := o.Reorder
		if span >= len(pkts) {
			span = len(pkts) - 1
		}
		for i := 0; i+span < len(pkts); i += span + 2 {
			pkts[i], pkts[i+span] = pkts[i+span], pkts[i]
		}
	}
	for _, p := range pkts {
		st.Packets = append(st.Packets, p.data)
	}
	return st, nil
}

func marshalRTP(o RTPOptions, seq uint16, ts uint32, payload []byte, marker bool) ([]byte, error) {
	p := &rtp.Packet{Header: rtp.Header{
		Version:        2,
		Marker:         marker,
		PayloadType:    o.PayloadType,
		SequenceNumber: seq,
		Timestamp:      ts,
		SSRC:           o.SSRC,
	}, Payload: payload}
	buf, err := p.Marshal()
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// BuildRTPWrap generates a stream whose sequence numbers cross 0xFFFF.
func BuildRTPWrap(o RTPOptions) (*RTPStream, error) {
	o.Samples = 40
	o.StartSeq = 65530
	o.LossBurst = 0
	o.Duplicate = 0
	o.Reorder = 0
	st, err := BuildRTP(o)
	if err != nil {
		return nil, err
	}
	st.Name = "rtp-wrap.bin"
	return st, nil
}

// EncodeRTPStream serialises packets with a 4-byte big-endian length prefix,
// matching the on-disk format the demo reads.
func EncodeRTPStream(packets [][]byte) []byte {
	var out []byte
	for _, p := range packets {
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(p)))
		out = append(out, hdr[:]...)
		out = append(out, p...)
	}
	return out
}
