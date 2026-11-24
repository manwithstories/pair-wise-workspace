package ingest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"mediarun/internal/model"
)

// Len is the number of events written but not yet replayed.
func (s *spillStore) Len() int { return s.wrote - s.readN }

// Read replays up to n events in write order.
func (s *spillStore) Read(n int) ([]model.Event, error) {
	if n <= 0 || s.Len() == 0 {
		return nil, nil
	}
	if err := s.openRead(); err != nil {
		return nil, err
	}
	if n > s.Len() {
		n = s.Len()
	}
	out := make([]model.Event, 0, n)
	for i := 0; i < n; i++ {
		line, err := s.reader.ReadBytes('\n')
		if err != nil {
			return out, fmt.Errorf("%w: spill truncated after %d events: %v",
				model.ErrTruncated, s.readN, err)
		}
		var ev model.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return out, fmt.Errorf("%w: spill decode: %v", model.ErrCorruptIndex, err)
		}
		out = append(out, ev)
		s.readN++
	}
	return out, nil
}

func (s *spillStore) openRead() error {
	if s.read != nil {
		return nil
	}
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	s.read = f
	s.reader = bufio.NewReader(f)
	return nil
}

// Close flushes and closes both handles.
func (s *spillStore) Close() error {
	var first error
	if s.write != nil {
		if err := s.write.Close(); err != nil {
			first = err
		}
		s.write = nil
	}
	if s.read != nil {
		if err := s.read.Close(); err != nil && first == nil {
			first = err
		}
		s.read = nil
		s.reader = nil
	}
	return first
}

// Remove deletes the journal; used after a successful commit so a rerun
// starts clean.
func (s *spillStore) Remove() error {
	if s.path == "" {
		return nil
	}
	if err := s.Close(); err != nil {
		return err
	}
	err := os.Remove(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	s.wrote, s.readN, s.bytes = 0, 0, 0
	return nil
}
