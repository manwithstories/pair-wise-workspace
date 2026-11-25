package main

import (
	"encoding/binary"
	"math/rand"

	"github.com/pion/rtp"

	"mediad/internal/clock"
	"mediad/internal/codec"
)

// trackSpec describes one synthetic source (studio, field, remote voice).
type trackSpec struct {
	id       string
	ssrc     uint32
	c        codec.Codec
	base     clock.TimeBase
	packets  int
	silentAt map[int]bool // packet indices carrying silence
	echo     map[int][]byte // packet index -> payload copied from another track
}

// pcmPayload builds a deterministic pseudo-PCM body behind an Opus TOC
// byte (or raw for AAC). amp controls loudness for silence detection.
func pcmPayload(r *rand.Rand, toc []byte, samples int, amp int16) []byte {
	out := make([]byte, len(toc)+2*samples)
	copy(out, toc)
	phase := r.Intn(1000)
	for i := 0; i < samples; i++ {
		// Deterministic tone: square-ish wave at the requested amplitude.
		v := amp
		if (i+phase)%32 >= 16 {
			v = -amp
		}
		binary.LittleEndian.PutUint16(out[len(toc)+2*i:], uint16(v))
	}
	return out
}

// genTrack builds one ordered packet stream. Opus uses 20 ms frames
// (960 samples @48k) with an occasional 10 ms frame and one zero-duration
// keep-alive; AAC uses 1024-sample frames @44.1k.
func genTrack(spec trackSpec, r *rand.Rand) []*rtp.Packet {
	var pkts []*rtp.Packet
	var ts uint32
	for i := 0; i < spec.packets; i++ {
		amp := int16(12000)
		if spec.silentAt[i] {
			amp = 40
		}
		var payload []byte
		dur := uint32(960)
		switch spec.c {
		case codec.Opus:
			toc := byte(1 << 3) // config 1: 20 ms
			if i%9 == 4 {
				toc = byte(0 << 3) // config 0: 10 ms
				dur = 480
			}
			if i%17 == 8 { // zero-duration keep-alive
				payload = nil
				dur = 0
			} else if echo, ok := spec.echo[i]; ok {
				payload = echo
			} else {
				payload = pcmPayload(r, []byte{toc}, 480, amp)
			}
		case codec.AAC:
			dur = 1024
			payload = pcmPayload(r, nil, 512, amp)
		}
		pkts = append(pkts, &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				SequenceNumber: uint16(i),
				Timestamp:      ts,
				SSRC:           spec.ssrc,
			},
			Payload: payload,
		})
		ts += dur
	}
	return pkts
}

// faultSpec describes the weak-network fault suite injected into a stream.
type faultSpec struct {
	duplicateSeqs []uint16  // identical retransmissions
	reorderAt     []int     // swap positions (arrival order)
	burstStart    uint16    // first dropped seq
	burstCount    int       // dropped burst length
	conflictSeq   int       // re-sent with a different payload (-1: none)
	lateSeq       int       // delivered after the window closed (-1: none)
	bandwidth     []float64 // bandwidth trace applied while pushing
}

// injectFaults turns an ordered stream into an arrival-order list with
// duplicates, reordering, burst loss, a conflict and a late packet.
func injectFaults(pkts []*rtp.Packet, f faultSpec) []*rtp.Packet {
	bySeq := map[uint16]*rtp.Packet{}
	var arrival []*rtp.Packet
	var late *rtp.Packet
	for _, p := range pkts {
		bySeq[p.SequenceNumber] = p
		if f.lateSeq >= 0 && int(p.SequenceNumber) == f.lateSeq {
			late = p // held back, delivered after the window closes
			continue
		}
		if p.SequenceNumber >= f.burstStart &&
			int(p.SequenceNumber-f.burstStart) < f.burstCount {
			continue // burst loss
		}
		arrival = append(arrival, p)
	}
	// Reorder: swap neighbours at the requested positions.
	for _, i := range f.reorderAt {
		if i+1 < len(arrival) {
			arrival[i], arrival[i+1] = arrival[i+1], arrival[i]
		}
	}
	// Duplicates and the conflict packet ride at the end of their burst.
	var out []*rtp.Packet
	for _, p := range arrival {
		out = append(out, p)
		for _, d := range f.duplicateSeqs {
			if p.SequenceNumber == d {
				out = append(out, p) // identical duplicate
			}
		}
		if f.conflictSeq >= 0 && int(p.SequenceNumber) == f.conflictSeq {
			evil := *p
			evil.Payload = append([]byte(nil), p.Payload...)
			if len(evil.Payload) > 0 {
				evil.Payload[len(evil.Payload)-1] ^= 0xFF // same seq, new payload
			}
			out = append(out, &evil)
		}
	}
	if late != nil {
		out = append(out, late) // beyond the wait window: unrecoverable
	}
	return out
}

// defaultTracks returns the three-track production scenario: 1.2 s of
// audio per track, cut into 12 segments of 100 ms.
func defaultTracks() []trackSpec {
	silent := map[int]bool{}
	for i := 20; i < 25; i++ { // 400..500 ms: silent stretch on studio
		silent[i] = true
	}
	return []trackSpec{
		{id: "studio", ssrc: 1001, c: codec.Opus, base: clock.Base48k,
			packets: 60, silentAt: silent},
		{id: "field", ssrc: 2002, c: codec.AAC, base: clock.Base44k,
			packets: 51, silentAt: map[int]bool{}},
		{id: "remote", ssrc: 3003, c: codec.Opus, base: clock.Base48k,
			packets: 60, silentAt: map[int]bool{}},
	}
}
