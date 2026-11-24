package ingest

import (
	"fmt"

	"mediarun/internal/model"
	rtporder "mediarun/internal/rtp"
)

// fold converts delivered packets and gaps into backlog events.
func (s *Stream) fold(res rtporder.Result, rec *Receipt) error {
	for _, g := range res.Gaps {
		s.stats.Gaps++
		s.stats.GapPackets += g.Count
		s.pending = append(s.pending, model.Gap{
			TrackID: s.cfg.TrackID,
			From:    g.From,
			To:      g.To,
			Count:   g.Count,
		})
	}
	for _, p := range res.Ordered {
		tick, err := s.base.Tick(p.Timestamp)
		if err != nil {
			// A timestamp that cannot be placed on the unified clock is
			// corrupt input. It is reported, not silently dropped.
			s.stats.Dropped++
			return model.Classify(model.ClassCorrupt,
				fmt.Errorf("ingest: track %d seq %d timestamp: %w", s.cfg.TrackID, p.Sequence, err))
		}
		// Attach any holes that opened before this packet.
		if len(s.pending) > 0 {
			for i := range s.pending {
				g := &s.pending[i]
				start := s.lastEnd
				if !s.haveLast {
					start = tick
				}
				g.Start = start
				g.End = tick
				if g.End < g.Start {
					g.End = g.Start
				}
				rec.Gaps = append(rec.Gaps, *g)
			}
			s.pending = s.pending[:0]
		}
		ev := model.Event{
			Kind:      model.KindFrame,
			TrackID:   s.cfg.TrackID,
			TrackTime: p.Timestamp,
			Start:     s.cfg.StartTick + tick,
			Payload:   p.Payload,
			Sequence:  uint32(p.Sequence),
			Keyframe:  p.Marker,
		}
		s.backlog = append(s.backlog, ev)
		s.stats.Delivered++
		s.lastEnd = s.cfg.StartTick + tick + 1
		s.haveLast = true
	}
	return nil
}
