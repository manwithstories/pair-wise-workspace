package pipeline

import (
	"github.com/pion/rtp"

	"relaycheck/internal/audio"
	"relaycheck/internal/media"
	"relaycheck/internal/transport"
)

// RTPPlan is the deterministic impaired stream and its resolved gaps.
type RTPPlan struct {
	BurstStart int
	Arrivals   []*rtp.Packet
	Gaps       []transport.Gap
	Stats      transport.Stats
	SegGaps    [][]audio.FrameGap
}

// buildRTPPlan packetizes the shared 48k timeline, applies deterministic
// reordering/duplication/burst-loss and runs the wrap-aware reorder buffer.
func buildRTPPlan(cfg Config, clock *media.Clock) (*RTPPlan, *media.Fault) {
	total := SegmentCount * BlocksPerSegment
	ssrc := uint32(cfg.Seed*2654435761 + 7)

	base := make([]*rtp.Packet, total)
	for g := 0; g < total; g++ {
		base[g] = &rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: 96, SSRC: ssrc,
				SequenceNumber: uint16(g),
				Timestamp:      uint32(g * FramesPerBlock),
			},
			Payload: []byte{byte(g >> 8), byte(g), byte(BlocksPerSegment)},
		}
	}

	// Deterministic small displacement (2) swaps, well inside the window.
	for k := 4; k+2 < total; k += 11 {
		base[k], base[k+2] = base[k+2], base[k]
	}

	seedMod := ((cfg.Seed % 100) + 100) % 100
	burstSegment := 2 + int(seedMod)
	burstStart := burstSegment*BlocksPerSegment + 3
	lost := map[uint16]bool{}
	for b := 0; b < cfg.BurstLoss; b++ {
		lost[uint16(burstStart+b)] = true
	}

	dupCap := 0
	if cfg.DuplicateEvery > 0 {
		dupCap = total / cfg.DuplicateEvery
	}
	arrivals := make([]*rtp.Packet, 0, total+dupCap+1)
	for _, p := range base {
		if lost[p.SequenceNumber] {
			continue // burst removes originals and (below) any duplicates
		}
		arrivals = append(arrivals, p)
		if cfg.DuplicateEvery > 0 && (int(p.SequenceNumber)+1)%cfg.DuplicateEvery == 0 {
			dup := *p // exact retransmitted copy: same seq and timestamp
			arrivals = append(arrivals, &dup)
		}
	}

	_, gaps, stats, f := transport.ReorderAll(arrivals, cfg.ReorderWindow)
	if f != nil {
		return nil, f
	}

	segGaps := make([][]audio.FrameGap, SegmentCount)
	for _, gp := range gaps {
		for s := int(gp.FromSeq); s <= int(gp.ToSeq); s++ {
			block := s % BlocksPerSegment
			segIdx := s / BlocksPerSegment
			if segIdx < 0 || segIdx >= SegmentCount {
				continue
			}
			startFrame := block * FramesPerBlock
			endFrame := startFrame + FramesPerBlock
			gr, rf := clock.Range(int64(s)*FramesPerBlock, int64(s+1)*FramesPerBlock)
			if rf != nil {
				return nil, rf
			}
			segGaps[segIdx] = append(segGaps[segIdx], audio.FrameGap{
				StartFrame: startFrame, EndFrame: endFrame, Range: gr,
			})
		}
	}

	return &RTPPlan{
		BurstStart: burstStart, Arrivals: arrivals,
		Gaps: gaps, Stats: stats, SegGaps: segGaps,
	}, nil
}
