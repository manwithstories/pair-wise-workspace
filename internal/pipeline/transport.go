package pipeline

import (
	"context"

	"github.com/pairwise/relay/internal/media"
	"github.com/pairwise/relay/internal/transport"
)

type resolvedRung struct {
	input       RungInput
	data        map[uint64][]byte
	source      map[uint64]string
	gaps        map[uint64]bool
	gapSegments []uint64
	stats       transport.Stats
}

func resolveTransport(ctx context.Context, in Input) ([]*resolvedRung, transport.Stats, error) {
	out := make([]*resolvedRung, 0, len(in.RungInputs))
	var aggregate transport.Stats
	for _, ri := range in.RungInputs {
		if err := ctx.Err(); err != nil {
			return nil, aggregate, err
		}
		window := in.Window
		if window == 0 {
			window = 8
		}
		r, err := transport.NewReorderer(window, int(window)*4+16, 1)
		if err != nil {
			return nil, aggregate, err
		}
		rr := &resolvedRung{
			input:  ri,
			data:   make(map[uint64][]byte, in.Segments),
			source: make(map[uint64]string, in.Segments),
			gaps:   make(map[uint64]bool),
		}
		collect := func(ps []transport.Packet) {
			for _, p := range ps {
				rr.data[p.ID.Sequence] = p.Data
				rr.source[p.ID.Sequence] = p.SourceNode
			}
		}

		for _, p := range ri.Packets {
			res := r.Push(p)
			if res.Fault != nil && !res.Duplicate && !res.Late && !res.Outside {
				return nil, aggregate, res.Fault
			}
			collect(res.Ordered)
		}

		trackKey := transport.TrackKey(in.StreamID, ri.ID)
		missing := r.Missing(trackKey, in.StreamID, ri.ID, uint64(in.Segments))
		items := make([]transport.RecoveryItem, 0, len(missing))
		for _, id := range missing {
			_, unavailable := ri.Unavailable[id.Sequence]
			_, hasIntact := ri.Intact[id.Sequence]
			items = append(items, transport.RecoveryItem{
				ID:          id,
				Bytes:       int64(len(ri.Intact[id.Sequence])),
				Unavailable: unavailable || !hasIntact,
			})
		}
		plan := transport.PlanRecovery(items, in.Bandwidth, in.Budget)
		recovered := make(map[uint64]bool)
		for _, id := range plan.Recovered {
			raw := ri.Intact[id.Sequence]
			if raw == nil {
				return nil, aggregate, media.NewFault(media.CodeUnrecoverable,
					media.FragmentID{Rendition: ri.ID, Segment: id.Sequence}, -1, false,
					"recovery planner selected packet but no intact refetch copy exists")
			}
			p := transport.Packet{
				ID: id, SourceNode: ri.SourceNode, Data: raw,
				Bytes: int64(len(raw)), Retransmission: true,
			}
			ordered, f := r.Recover(p)
			if f != nil {
				return nil, aggregate, f
			}
			recovered[id.Sequence] = true
			collect(ordered)
			rr.data[id.Sequence] = raw
			rr.source[id.Sequence] = ri.SourceNode
		}

		// Close still-missing sequences in ascending order. CloseGap releases
		// buffered later packets as each contiguous hole is skipped.
		for _, id := range missing {
			if recovered[id.Sequence] {
				continue
			}
			ordered := r.CloseGap(id, in.Budget.MaxAttempts)
			collect(ordered)
		}
		for seq := uint64(1); seq <= uint64(in.Segments); seq++ {
			if rr.data[seq] == nil {
				rr.gaps[seq] = true
				rr.gapSegments = append(rr.gapSegments, seq)
			}
		}
		rr.stats = r.Stats()
		aggregate.Packets += rr.stats.Packets
		aggregate.Duplicates += rr.stats.Duplicates
		aggregate.OutOfOrder += rr.stats.OutOfOrder
		aggregate.Late += rr.stats.Late
		aggregate.Missing += rr.stats.Missing
		aggregate.Recovered += rr.stats.Recovered
		aggregate.Gaps = append(aggregate.Gaps, rr.stats.Gaps...)
		out = append(out, rr)
	}
	return out, aggregate, nil
}
