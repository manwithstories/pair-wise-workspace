package fixture

import (
	"github.com/pion/rtp"
)

type lossMode int

const (
	lossModeSingle lossMode = iota
	lossModeBurst
)

// vfrVideoTrack defines irregular sample durations (variable frame rate) on
// a 90kHz track; sizes stay small to keep fixtures compact.
func vfrVideoTrack() TrackSpec {
	return TrackSpec{
		ID:        1,
		Kind:      Video,
		Timescale: 90000,
		Samples: []SampleSpec{
			{Duration: 3003, Size: 40}, // ~33.37ms
			{Duration: 1001, Size: 24}, // ~11.12ms (VFR spike)
			{Duration: 2002, Size: 32}, // ~22.24ms
			{Duration: 3003, Size: 40},
			{Duration: 1001, Size: 24},
			{Duration: 2002, Size: 32},
		},
	}
}

// audioTrack uses a constant 48kHz cadence (1024-tick AAC frames).
func audioTrack() TrackSpec {
	samples := make([]SampleSpec, 8)
	for i := range samples {
		samples[i] = SampleSpec{Duration: 1024, Size: 16}
	}
	return TrackSpec{ID: 2, Kind: Audio, Timescale: 48000, Samples: samples}
}

// buildMultiFragmentStream emits one video fragment followed by one audio
// fragment so both tracks share the same stream and clock.
func buildMultiFragmentStream(video, audio TrackSpec) []byte {
	videoFrag := BuildFragment(1, video)
	audioFrag := BuildFragment(2, audio)
	out := make([]byte, 0, len(videoFrag)+len(audioFrag))
	out = append(out, videoFrag...)
	out = append(out, audioFrag...)
	return out
}

// buildWire RTP-packetizes the CMAF bytes into fixed-size payload chunks,
// generates one XOR parity block per groupSize packets, then applies the
// deterministic reorder/duplicate/loss plan.
func buildWire(pb *planBuilder, cmaf []byte, seed int64, mode lossMode) ([]Packet, []FECSpec, SequencePlan) {
	var mediaPkts []*rtp.Packet
	var seq uint16
	for off := 0; off < len(cmaf); off += mtu {
		end := off + mtu
		if end > len(cmaf) {
			end = len(cmaf)
		}
		mediaPkts = append(mediaPkts, rtpBuild(videoSSRC, seq, uint32(off), cmaf[off:end]))
		seq++
	}

	// Local FEC: one parity packet per contiguous group.
	var fecSpecs []FECSpec
	var fecPkts []Packet
	allSeqs := make([]uint16, 0, len(mediaPkts))
	for _, p := range mediaPkts {
		allSeqs = append(allSeqs, p.SequenceNumber)
	}
	var groups [][]uint16
	for i := 0; i < len(mediaPkts); i += groupSize {
		hi := i + groupSize
		if hi > len(mediaPkts) {
			hi = len(mediaPkts)
		}
		group := mediaPkts[i:hi]
		parity := xorPayloads(group)
		spec := FECSpec{FirstSeq: group[0].SequenceNumber, LastSeq: group[len(group)-1].SequenceNumber, Parity: parity}
		fecSpecs = append(fecSpecs, spec)
		groups = append(groups, seqsOf(group))
		fecPkt := rtpBuild(videoSSRC, seq, 0, parity)
		fecPkts = append(fecPkts, Packet{Seq: seq, Packet: fecPkt, IsFEC: true})
		seq++
	}

	plan := SequencePlan{FECGroups: groups}
	injectLosses(pb, allSeqs, mode, &plan)

	// Reorder the surviving media packets and add a deterministic duplicate.
	survivors := removeU16(allSeqs, append(append([]uint16{}, plan.SingleLosses...), plan.BurstLosses...))
	order := pb.shuffle(survivors)
	plan.Order = order
	if len(survivors) >= 3 {
		dup := survivors[pb.rng.Intn(len(survivors))]
		plan.Duplicates = []uint16{dup}
		order = insertDuplicate(order, dup, pb.rng.Intn(len(order)+1))
	}

	bySeq := map[uint16]*rtp.Packet{}
	for _, p := range mediaPkts {
		bySeq[p.SequenceNumber] = p
	}
	var out []Packet
	for _, s := range order {
		out = append(out, Packet{Seq: s, Packet: bySeq[s]})
	}
	out = append(out, fecPkts...)
	return out, fecSpecs, plan
}

func injectLosses(pb *planBuilder, seqs []uint16, mode lossMode, plan *SequencePlan) {
	switch mode {
	case lossModeSingle:
		// One isolated loss chosen from the first full FEC group.
		pool := seqs
		if len(pool) > groupSize {
			pool = seqs[:groupSize]
		}
		plan.SingleLosses = pb.pick(pool, 1)
	case lossModeBurst:
		// A contiguous two-packet burst inside one group: not XOR-repairable.
		pool := seqs
		if len(pool) > groupSize {
			pool = seqs[:groupSize]
		}
		plan.BurstLosses = pb.burst(pool, 2)
	}
}

func insertDuplicate(order []uint16, dup uint16, at int) []uint16 {
	out := make([]uint16, 0, len(order)+1)
	out = append(out, order[:at]...)
	out = append(out, dup)
	out = append(out, order[at:]...)
	return out
}

func seqsOf(pkts []*rtp.Packet) []uint16 {
	out := make([]uint16, len(pkts))
	for i, p := range pkts {
		out[i] = p.SequenceNumber
	}
	return out
}

func xorPayloads(pkts []*rtp.Packet) []byte {
	maxLen := 0
	for _, p := range pkts {
		if len(p.Payload) > maxLen {
			maxLen = len(p.Payload)
		}
	}
	out := make([]byte, maxLen)
	for _, p := range pkts {
		for i, b := range p.Payload {
			out[i] ^= b
		}
	}
	return out
}
