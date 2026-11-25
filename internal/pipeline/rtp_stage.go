package pipeline

import (
	"context"

	pionrtp "github.com/pion/rtp"

	"avpipe/internal/media"
	"avpipe/internal/rtp"
)

// rtpCounters aggregates the transport-stage outcome.
type rtpCounters struct {
	lost            int
	lostMedia       int
	recovered       int
	unrecoverable   int
	fecFailedGroups int
	overflows       int
	late            int
}

// runRTP packetizes good frames, adds XOR FEC, simulates the lossy path, then
// reorders through the jitter buffer and reconstructs single-packet losses.
// It returns per-packet latency samples (arrival vs. RTP timestamp, in ms)
// and the transport counters.
func (e *Engine) runRTP(ctx context.Context, frames []media.Frame) ([]float64, rtpCounters, error) {
	if err := ctx.Err(); err != nil {
		return nil, rtpCounters{}, media.NewError("pipeline", media.KindCancelled, 0,
			media.ActionCancel, "%v", err)
	}
	mediaPkts := rtp.Packetize(frames, 0)
	stream := rtp.EncodeFEC(mediaPkts)

	netCfg := rtp.NetworkConfig{
		LossRate:    e.cfg.LossRate,
		Burst:       e.cfg.Burst,
		JitterTicks: e.cfg.JitterTicks90k,
		Seed:        e.cfg.Seed,
	}
	arrivals := rtp.SimulateNetwork(stream, netCfg)

	// FEC groups are built from the authoritative media packet list, so their
	// base/size are always correct even when some packets never arrive.
	groups := buildGroups(mediaPkts)
	jb := rtp.NewJitterBuffer(e.cfg.JitterCapacity, e.cfg.JitterWindow90k)
	var counters rtpCounters
	var latencies []float64

	for _, ar := range arrivals {
		if ar.Lost {
			counters.lost++
			if _, _, isParity := rtp.ParseParity(ar.Packet); !isParity {
				counters.lostMedia++
			}
			continue
		}
		p := ar.Packet
		if base, _, isParity := rtp.ParseParity(p); isParity {
			if g := groups[base]; g != nil {
				g.Parity = p
			}
			continue // parity never enters the playout buffer
		}
		if g := groups[groupBase(p.SequenceNumber)]; g != nil {
			g.Present[p.SequenceNumber] = p
		}
		if err := jb.Push(p); err != nil {
			if me, ok := err.(*media.Error); ok && me.Kind == media.KindLimitExceeded {
				counters.overflows++
				continue
			}
			return nil, counters, err
		}
		latency := float64(media.ToMillis(ar.Arrive90k)) -
			float64(media.ToMillis(uint64(p.Timestamp)))
		if latency < 0 {
			latency = 0
		}
		latencies = append(latencies, latency)
	}

	groupList := make([]rtp.FECGroup, 0, len(groups))
	for _, g := range groups {
		groupList = append(groupList, *g)
	}
	recovered, failedGroups := rtp.DecodeFEC(groupList)
	counters.recovered = len(recovered)
	counters.fecFailedGroups = failedGroups
	for _, p := range recovered {
		_ = jb.Push(p) // rebuilt packets may arrive late; buffer handles it
	}

	// End-of-stream settlement: flush every remaining packet and fill every
	// still-missing sequence with an explicit gap. The gap count is the
	// authoritative measure of media packets neither received nor rebuilt.
	for _, po := range jb.Flush(uint16(len(mediaPkts))) {
		if po.Gap {
			counters.unrecoverable++
		}
	}
	counters.late, counters.overflows = jb.Stats()
	return latencies, counters, nil
}

// buildGroups constructs one FEC group per consecutive FECGroupSize media
// packets, with a final partial group when needed.
func buildGroups(mediaPkts []*pionrtp.Packet) map[uint16]*rtp.FECGroup {
	groups := make(map[uint16]*rtp.FECGroup)
	for i := 0; i < len(mediaPkts); i += rtp.FECGroupSize {
		end := i + rtp.FECGroupSize
		if end > len(mediaPkts) {
			end = len(mediaPkts)
		}
		base := mediaPkts[i].SequenceNumber
		groups[base] = &rtp.FECGroup{
			Base:    base,
			Size:    end - i,
			Present: map[uint16]*pionrtp.Packet{},
		}
	}
	return groups
}

// groupBase returns the base sequence of the FEC group covering seq.
func groupBase(seq uint16) uint16 {
	return seq - seq%rtp.FECGroupSize
}
