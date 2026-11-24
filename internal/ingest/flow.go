package ingest

import (
	"fmt"

	"mediarun/internal/model"
)

// SetBandwidth adapts the batch size and backlog cap to a changing link.
// mbps must be positive. The new cap is applied to future backlogs; already
// spilled events remain on disk.
func (s *Stream) SetBandwidth(mbps int) error {
	if mbps <= 0 || mbps > 10000 {
		return fmt.Errorf("%w: bandwidth %d", model.ErrInvalid, mbps)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := mbps * 8
	if batch < MinBatch {
		batch = MinBatch
	}
	if batch > MaxBatch {
		batch = MaxBatch
	}
	s.batch = batch
	cap := mbps * 256
	if cap < MinBatch {
		cap = MinBatch
	}
	if cap > s.cfg.MaxBacklog*8 {
		cap = s.cfg.MaxBacklog * 8
	}
	s.backlogCap = cap
	return nil
}

// Batch reports the current receive batch size.
func (s *Stream) Batch() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.batch
}

// BacklogCap reports the current in-memory backlog limit.
func (s *Stream) BacklogCap() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backlogCap
}

// Stats returns a snapshot.
func (s *Stream) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}
