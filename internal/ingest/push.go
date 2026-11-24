package ingest

import (
	"fmt"

	"mediarun/internal/model"
	rtporder "mediarun/internal/rtp"
)

// Push accepts an already decoded packet. Delivered packets are converted to
// unified-clock events and appended to the backlog.
func (s *Stream) Push(p rtporder.Packet) (Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Receipt{}, ErrClosed
	}
	if p.TrackID != 0 && p.TrackID != s.cfg.TrackID {
		return Receipt{}, fmt.Errorf("%w: foreign track %d", model.ErrInvalid, p.TrackID)
	}
	s.stats.Received++
	res := s.reorder.Push(p)
	rec := Receipt{TrackID: s.cfg.TrackID, Sequence: p.Sequence, Duplicate: res.Duplicate, TooOld: res.TooOld}
	if res.Duplicate {
		s.stats.Duplicates++
		return rec, nil
	}
	if res.TooOld {
		s.stats.TooOld++
		return rec, nil
	}
	s.stats.Accepted++
	if err := s.fold(res, &rec); err != nil {
		return rec, err
	}
	rec.Spilled = s.enforceBacklog()
	return rec, s.spillErr
}
