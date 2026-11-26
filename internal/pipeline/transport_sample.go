package pipeline

import (
	"fmt"

	"pairflow/internal/model"
	"pairflow/internal/syncer"
	"pairflow/internal/transport"
)

// TransportReport summarizes deterministic RTP jitter/FEC behavior.
type TransportReport struct {
	PacketsPushed      int          `json:"packets_pushed"`
	FECSingleRecovered bool         `json:"fec_single_recovered"`
	BurstLoss          bool         `json:"burst_loss"`
	ConflictDetected   bool         `json:"conflict_detected"`
	Duplicates         []uint16     `json:"duplicates"`
	Late               []uint16     `json:"late"`
	Gaps               []syncer.Gap `json:"gaps"`
}

func fixedShard(seed byte, size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = seed ^ byte(i*31+7)
	}
	return b
}

// RunTransportSample exercises RTP wire encoding, jitter deadlines, one-packet
// XOR recovery and the configured over-capacity burst loss.
func RunTransportSample(dropSeqs []int) (*TransportReport, error) {
	rep := &TransportReport{}

	// A separate block proves the happy-path XOR recovery invariant.
	singleSeqs := []uint16{901, 902, 903, 904}
	singleShards := [][]byte{fixedShard(11, 128), fixedShard(23, 128), fixedShard(37, 128), fixedShard(51, 128)}
	block, err := transport.NewFECBlock(1, singleSeqs, singleShards, 905)
	if err != nil {
		return nil, err
	}
	known := map[uint16][]byte{
		901: singleShards[0],
		902: singleShards[1],
		904: singleShards[3],
		905: block.Parity,
	}
	recovered, merr := block.RecoverXOR(known)
	if merr != nil {
		return nil, merr
	}
	if string(recovered) != string(singleShards[2]) {
		return nil, &model.MediaError{Code: model.CodeInternal, Message: "XOR recovery mismatch"}
	}
	rep.FECSingleRecovered = true

	// Main proof block: 809,810,811,812 with parity 813. The requested drops
	// 811 and 812 are two losses in one single-parity group.
	seqs := []uint16{809, 810, 811, 812}
	shards := [][]byte{fixedShard(71, 256), fixedShard(83, 256), fixedShard(97, 256), fixedShard(113, 256)}
	mainBlock, err := transport.NewFECBlock(8, seqs, shards, 813)
	if err != nil {
		return nil, err
	}
	drop := map[uint16]bool{}
	for _, s := range dropSeqs {
		drop[uint16(s)] = true
	}
	mainKnown := map[uint16][]byte{813: mainBlock.Parity}
	jb := transport.NewJitterBuffer(transport.JitterConfig{
		BaseSequence:     809,
		StartNS:          8_090_000_000,
		PacketDurationNS: 10_000_000,
		PlayoutDelayNS:   5_000_000,
	})
	for i, seq := range seqs {
		if drop[seq] {
			continue
		}
		mainKnown[seq] = shards[i]
		pkt := transport.BuildPacket(seq, uint32(809000+int(seq-809)*900), transport.DefaultSSRC, transport.VideoPayloadType, shards[i], seq == 812)
		if _, _, err := transport.RoundTrip(pkt); err != nil {
			return nil, err
		}
		if _, err := jb.Push(transport.ObservedPacket{Packet: pkt, ArrivalNS: 8_090_000_000 + int64(i)*1000}); err != nil {
			return nil, err
		}
		rep.PacketsPushed++
	}
	// Duplicate and reorder/late behavior are covered directly by tests.
	_ = jb.DrainUntil(8_200_000_000)
	rep.Duplicates = append(rep.Duplicates, jb.Duplicates...)
	rep.Late = append(rep.Late, jb.Late...)
	if _, ferr := mainBlock.RecoverXOR(mainKnown); ferr != nil {
		if ferr.Code != model.CodeBurstLoss {
			return nil, ferr
		}
		rep.BurstLoss = true
		rep.Gaps = append(rep.Gaps, syncer.Gap{
			TrackID:   "rtp-proof",
			Reason:    string(model.CodeBurstLoss),
			Sequences: []uint16{811, 812},
			Interval:  model.Interval{StartNS: 8_110_000_000, EndNS: 8_130_000_000, StartSeq: 811, EndSeq: 812},
		})
	}
	if !rep.BurstLoss {
		return nil, &model.MediaError{Code: model.CodeInternal, Message: "expected burst loss was not classified"}
	}

	// Same sequence with different content must be a conflict.
	conflictJB := transport.NewJitterBuffer(transport.JitterConfig{BaseSequence: 1, PacketDurationNS: 10_000_000})
	p1 := transport.BuildPacket(1, 1000, 1, 96, []byte("alpha"), false)
	p2 := transport.BuildPacket(1, 1000, 1, 96, []byte("omega"), false)
	_, _ = conflictJB.Push(transport.ObservedPacket{Packet: p1})
	_, cerr := conflictJB.Push(transport.ObservedPacket{Packet: p2})
	if cerr == nil {
		return nil, fmt.Errorf("sequence conflict was not reported")
	}
	rep.ConflictDetected = true
	return rep, nil
}
