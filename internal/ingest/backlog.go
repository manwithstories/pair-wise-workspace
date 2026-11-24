package ingest

import (
	"mediarun/internal/model"
)

// Drain returns up to n events, replaying the on-disk spill first so global
// order is preserved. n <= 0 returns everything buffered.
func (s *Stream) Drain(n int) ([]model.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := n
	if limit <= 0 || limit > s.batch*4 {
		limit = s.batch * 4
	}
	var out []model.Event
	if s.spill != nil && s.spill.Len() > 0 {
		evs, err := s.spill.Read(limit)
		if err != nil {
			return nil, err
		}
		out = append(out, evs...)
		s.stats.Replayed += len(evs)
	}
	if len(out) < limit {
		take := limit - len(out)
		if take > len(s.backlog) {
			take = len(s.backlog)
		}
		out = append(out, s.backlog[:take]...)
		s.backlog = s.backlog[take:]
	}
	return out, nil
}

// Close flushes the reorder buffer, folds the remaining packets and closes
// the spill file. Close is idempotent.
func (s *Stream) Close() (Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Receipt{}, nil
	}
	s.closed = true
	res := s.reorder.Close()
	rec := Receipt{TrackID: s.cfg.TrackID}
	if err := s.fold(res, &rec); err != nil {
		return rec, err
	}
	// Holes that never saw a following packet still belong in the
	// verification summary; they end at the last observed position.
	for _, g := range s.pending {
		g.Start = s.lastEnd
		g.End = s.lastEnd
		rec.Gaps = append(rec.Gaps, g)
	}
	s.pending = nil
	rec.Spilled = s.enforceBacklog()
	if s.spill != nil {
		if err := s.spill.Close(); err != nil {
			return rec, err
		}
	}
	return rec, s.spillErr
}

// Pending reports how many events are still buffered in memory.
func (s *Stream) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.backlog)
}

// Spilled reports how many events are currently on disk.
func (s *Stream) Spilled() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spill == nil {
		return 0
	}
	return s.spill.Len()
}
