package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// WriteArtifact stores a stage artifact and returns its content hash.
func (s *Store) WriteArtifact(stage string, data []byte) (string, error) {
	name := fmt.Sprintf("stage_%s.json", sanitize(stage))
	if err := AtomicWrite(filepath.Join(s.Dir, name), data, 0o644); err != nil {
		return "", err
	}
	return sha256Hex(data), nil
}

// ReadArtifact loads and verifies a stage artifact.
func (s *Store) ReadArtifact(stage, want string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, fmt.Sprintf("stage_%s.json", sanitize(stage))))
	if err != nil {
		return nil, err
	}
	if got := sha256Hex(data); got != want {
		return nil, fmt.Errorf("%w: stage %s hash %s != %s", ErrCorruptCheckpoint, stage, got, want)
	}
	return data, nil
}

// AppendRecord appends one JSONL trace record with an fsync.
func (s *Store) AppendRecord(rec Record) error {
	rec.Version = Version
	rec.Time = time.Now().UTC()
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, "journal.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// Records reads all trace records in append order.
func (s *Store) Records() ([]Record, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, "journal.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Record
	start := 0
	for i := 0; i <= len(data); i++ {
		if i < len(data) && data[i] != '\n' {
			continue
		}
		line := data[start:i]
		start = i + 1
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("%w: journal line: %v", ErrCorruptCheckpoint, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// Reused returns completed stages in canonical order that still have
// verifiable artifacts for the given input digest.
func (s *Store) Reused(inputDigest string) []string {
	cp, err := s.LoadCheckpoint()
	if err != nil || cp == nil || cp.InputDigest != inputDigest {
		return nil
	}
	var out []string
	for _, stage := range cp.Completed {
		hash, ok := cp.Hashes[stage]
		if !ok {
			continue
		}
		if _, err := s.ReadArtifact(stage, hash); err == nil {
			out = append(out, stage)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return stageIndex(out[i]) < stageIndex(out[j]) })
	return out
}

func stageIndex(stage string) int {
	for i, s := range []string{"parse", "recover", "repair", "index", "finalize"} {
		if s == stage {
			return i
		}
	}
	return len([]string{"parse", "recover", "repair", "index", "finalize"})
}

func sanitize(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			out = append(out, r)
		}
	}
	return string(out)
}
