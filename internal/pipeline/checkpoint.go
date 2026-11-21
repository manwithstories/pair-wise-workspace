package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"mediacheck/internal/fault"
)

// State is the atomic resume state: input hash, last committed chunk
// and parse status. Written via temp-file + rename so a crash never
// leaves a half-written checkpoint.
type State struct {
	InputSHA256 string        `json:"input_sha256"`
	ChunkSize   int64         `json:"chunk_size"`
	QueueCap    int           `json:"queue_cap"`
	Committed   []SegmentHash `json:"committed"`
	Done        bool          `json:"done"`

	verified bool // all pre-existing committed hashes re-matched
}

// loadCheckpoint reads existing state; a missing file is a fresh run.
// A mismatched chunk size or queue cap is fatal: segment boundaries
// would differ from the committed ones.
func loadCheckpoint(path string, cfg Config) (*State, bool, error) {
	s := &State{ChunkSize: cfg.ChunkSize, QueueCap: cfg.QueueCap, verified: true}
	if path == "" {
		return s, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, false, nil
		}
		return nil, false, fault.New(fault.CatCheckpoint, -1, false,
			"read checkpoint: %v", err)
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, false, fault.New(fault.CatCheckpoint, -1, false,
			"corrupt checkpoint: %v", err)
	}
	if s.ChunkSize != cfg.ChunkSize || s.QueueCap != cfg.QueueCap {
		return nil, false, fault.New(fault.CatCheckpoint, -1, false,
			"checkpoint config mismatch (chunk %d→%d, queue %d→%d)",
			s.ChunkSize, cfg.ChunkSize, s.QueueCap, cfg.QueueCap)
	}
	s.verified = true
	return s, true, nil
}

// commit records one analyzed segment. Segments already present from
// a previous run are verified against the recomputed hash — identical
// boundaries and hashes are the resume-consistency evidence.
func (s *State) commit(seg SegmentHash, path string) error {
	if seg.Index < len(s.Committed) {
		prev := s.Committed[seg.Index]
		if prev != seg {
			s.verified = false
			return fault.New(fault.CatCheckpoint, seg.Offset, false,
				"segment %d differs from checkpoint (hash %s → %s)",
				seg.Index, prev.SHA256, seg.SHA256)
		}
		return nil // already committed: nothing to save
	}
	if seg.Index != len(s.Committed) {
		return fault.New(fault.CatCheckpoint, seg.Offset, false,
			"segment index gap: have %d, got %d", len(s.Committed), seg.Index)
	}
	s.Committed = append(s.Committed, seg)
	return s.save(path)
}

// inputHash derives a deterministic whole-input hash from the
// committed segment hashes.
func (s *State) inputHash() string {
	h := sha256.New()
	for _, c := range s.Committed {
		h.Write([]byte(c.SHA256))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// save writes state atomically (temp file in the same directory +
// rename). A no-op when path is empty.
func (s *State) save(path string) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path),
		"."+filepath.Base(path)+".tmp.*")
	if err != nil {
		return fault.New(fault.CatCheckpoint, -1, false,
			"checkpoint temp file: %v", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fault.New(fault.CatCheckpoint, -1, false,
			"checkpoint write: %v", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fault.New(fault.CatCheckpoint, -1, false,
			"checkpoint rename: %v", err)
	}
	return nil
}
