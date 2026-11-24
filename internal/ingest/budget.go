package ingest

// enforceBacklog spills the oldest events once the backlog exceeds its cap.
// Spilling is lossless: every event is written before it leaves memory.
func (s *Stream) enforceBacklog() int {
	if len(s.backlog) <= s.backlogCap {
		return 0
	}
	excess := len(s.backlog) - s.backlogCap
	if excess > s.batch {
		excess = s.batch
	}
	chunk := s.backlog[:excess]
	spilled, err := s.ensureSpill().Write(chunk)
	// Events the journal accepted have left memory; the rest stay put.
	if spilled > 0 {
		s.backlog = s.backlog[spilled:]
		s.stats.Spilled += spilled
	}
	if err != nil && s.spillErr == nil {
		s.spillErr = err
	}
	return spilled
}

func (s *Stream) ensureSpill() spillQueue {
	if s.spill == nil {
		s.spill = newSpillStore(s.cfg.SpillDir, s.cfg.SpillLimit)
	}
	return s.spill
}
