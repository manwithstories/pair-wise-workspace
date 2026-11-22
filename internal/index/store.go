// Package index maintains the local append-only scan index. Each chunk is
// written to a temp file then atomically renamed, so a crash never leaves
// a half-written index entry; committed chunks are never double counted.
package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Entry is one chunk's index record.
type Entry struct {
	Chunk      int    `json:"chunk"`
	FrameFrom  int    `json:"frame_from"`
	FrameTo    int    `json:"frame_to"`
	StartTick  int64  `json:"start_tick"`
	EndTick    int64  `json:"end_tick"`
	ValidFrames int   `json:"valid_frames"`
	TotalFrames int   `json:"total_frames"`
	CorruptRanges int `json:"corrupt_ranges"`
}

// Summary is the aggregate index state rebuilt from committed entries.
type Summary struct {
	Entries      []Entry
	Chunks       int
	TotalFrames  int
	ValidFrames  int
	FirstTick    int64
	LastTick     int64
	InputDigest  string
}

// Store is the file-backed chunk index.
type Store struct {
	mu       sync.Mutex
	dir      string
	committed map[int]Entry
	digest   string
}

const indexDirName = "chunks"

// ErrChunkExists is returned when a chunk number is committed twice.
var ErrChunkExists = errors.New("index: chunk already committed")

// Open loads any previously committed chunks from dir.
func Open(dir, inputDigest string) (*Store, error) {
	if err := safeMkdir(dir); err != nil {
		return nil, err
	}
	cdir := filepath.Join(dir, indexDirName)
	if err := safeMkdir(cdir); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, committed: map[int]Entry{}, digest: inputDigest}
	matches, err := filepath.Glob(filepath.Join(cdir, "chunk-*.json"))
	if err != nil {
		return nil, err
	}
	for _, m := range matches {
		var e Entry
		data, err := os.ReadFile(m)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("index: corrupt entry %s: %w", m, err)
		}
		s.committed[e.Chunk] = e
	}
	return s, nil
}

// safeMkdir rejects path traversal and creates dir. The raw path is
// inspected before Clean, which would otherwise resolve ".." away.
func safeMkdir(dir string) error {
	if dir == "" {
		return fmt.Errorf("index: empty path")
	}
	if containsDotDot(filepath.ToSlash(dir)) {
		return fmt.Errorf("index: path traversal rejected %q", dir)
	}
	return os.MkdirAll(filepath.Clean(dir), 0o755)
}

func containsDotDot(p string) bool {
	for _, part := range splitSlash(p) {
		if part == ".." {
			return true
		}
	}
	return false
}

func splitSlash(p string) []string {
	var out []string
	cur := ""
	for _, r := range p {
		if r == '/' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}

// Commit writes one chunk to a temp file and atomically renames it.
// A duplicate chunk number is rejected instead of being counted again.
func (s *Store) Commit(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.committed[e.Chunk]; ok {
		return fmt.Errorf("%w: chunk %d", ErrChunkExists, e.Chunk)
	}
	if e.FrameTo < e.FrameFrom || e.EndTick < e.StartTick || e.TotalFrames <= 0 {
		return fmt.Errorf("index: invalid entry %+v", e)
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	cdir := filepath.Join(s.dir, indexDirName)
	final := filepath.Join(cdir, fmt.Sprintf("chunk-%06d.json", e.Chunk))
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return err
	}
	s.committed[e.Chunk] = e
	return nil
}

// Committed reports whether a chunk number is already persisted.
func (s *Store) Committed(chunk int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.committed[chunk]
	return ok
}

// Summary rebuilds aggregate counters and time boundaries from entries.
func (s *Store) Summary() Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Summary{InputDigest: s.digest, FirstTick: -1}
	chunks := make([]int, 0, len(s.committed))
	for c := range s.committed {
		chunks = append(chunks, c)
	}
	sort.Ints(chunks)
	for _, c := range chunks {
		e := s.committed[c]
		out.Entries = append(out.Entries, e)
		out.Chunks++
		out.TotalFrames += e.TotalFrames
		out.ValidFrames += e.ValidFrames
		if out.FirstTick < 0 || e.StartTick < out.FirstTick {
			out.FirstTick = e.StartTick
		}
		if e.EndTick > out.LastTick {
			out.LastTick = e.EndTick
		}
	}
	return out
}

// Reset removes all chunk files (used by index-truncation rebuild tests).
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cdir := filepath.Join(s.dir, indexDirName)
	if err := os.RemoveAll(cdir); err != nil {
		return err
	}
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		return err
	}
	s.committed = map[int]Entry{}
	return nil
}
