package ingest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediarun/internal/model"
)

// spillStore is an append-only, FIFO journal of confirmed events that no
// longer fit in the in-memory backlog. Writing happens before the event
// leaves memory, so a spill can never lose a confirmed event. The byte cap
// is an explicit limit: exceeding it is reported as an error instead of
// dropping data.
type spillStore struct {
	path     string
	write    *os.File
	read     *os.File
	reader   *bufio.Reader
	wrote    int
	readN    int
	bytes    int
	maxBytes int
}

func newSpillStore(dir string, maxBytes int) *spillStore {
	path := ""
	if dir != "" {
		path = filepath.Join(dir, "ingest-spill.jsonl")
	}
	return &spillStore{path: path, maxBytes: maxBytes}
}

// Write appends events to the journal and returns how many were persisted.
func (s *spillStore) Write(events []model.Event) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	if err := s.openWrite(); err != nil {
		return 0, err
	}
	w := bufio.NewWriter(s.write)
	n := 0
	for _, ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			return n, fmt.Errorf("ingest: spill encode: %w", err)
		}
		if s.maxBytes > 0 && s.bytes+len(line)+1 > s.maxBytes {
			return n, fmt.Errorf("%w: spill journal %d bytes", model.ErrLimit, s.maxBytes)
		}
		if _, err := w.Write(line); err != nil {
			return n, err
		}
		if err := w.WriteByte('\n'); err != nil {
			return n, err
		}
		s.bytes += len(line) + 1
		n++
	}
	if err := w.Flush(); err != nil {
		return n, err
	}
	s.wrote += n
	return n, nil
}

func (s *spillStore) openWrite() error {
	if s.write != nil {
		return nil
	}
	if s.path == "" {
		return fmt.Errorf("%w: spill directory not configured", model.ErrInvalid)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	flags := os.O_CREATE | os.O_APPEND | os.O_WRONLY
	// A fresh journal must not replay another run's records: the first
	// write of a run truncates whatever a previous run left behind.
	if s.wrote == 0 && s.readN == 0 {
		flags = os.O_CREATE | os.O_TRUNC | os.O_WRONLY
	}
	f, err := os.OpenFile(s.path, flags, 0o644)
	if err != nil {
		return err
	}
	s.write = f
	return nil
}
