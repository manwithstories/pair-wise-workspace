package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Store couples the WAL with atomic artifact commits in one directory.
type Store struct {
	dir string
	wal *WAL
	rec *Recovery
}

// Open opens the store in dir, replays the WAL and rolls back any
// uncommitted (pending) stage artifacts left by a crash.
func Open(dir string) (*Store, int, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, 0, err
	}
	wal, rec, err := OpenWAL(dir)
	if err != nil {
		return nil, 0, err
	}
	s := &Store{dir: dir, wal: wal, rec: rec}
	rollbacks, err := s.rollbackPending()
	if err != nil {
		return nil, 0, err
	}
	return s, rollbacks, nil
}

// Recovery returns the replayed WAL state.
func (s *Store) Recovery() *Recovery { return s.rec }

// Done reports whether a stage was already committed (idempotent resume).
func (s *Store) Done(stage string) bool {
	for _, c := range s.rec.Committed {
		if c == stage {
			return true
		}
	}
	return false
}

// ArtifactPath is the committed path of a stage artifact.
func (s *Store) ArtifactPath(stage string) string {
	return filepath.Join(s.dir, stage+".out")
}

// Commit writes the artifact atomically (tmp+rename) between WAL stage and
// commit records. A crash before the commit record leaves the stage
// pending and the artifact is rolled back on next open.
func (s *Store) Commit(stage string, artifact []byte) error {
	if s.Done(stage) {
		return nil // idempotent: already committed
	}
	sum := sha256.Sum256(artifact)
	if err := s.wal.AppendStage(stage, hex.EncodeToString(sum[:])); err != nil {
		return err
	}
	tmp := s.ArtifactPath(stage) + ".tmp"
	if err := os.WriteFile(tmp, artifact, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.ArtifactPath(stage)); err != nil {
		return err
	}
	if err := s.wal.Commit(stage); err != nil {
		return err
	}
	s.rec.Committed = append(s.rec.Committed, stage)
	return nil
}

// WriteUncommitted writes a stage WAL record and the artifact without the
// commit record, simulating a crash mid-commit. The next Open rolls it
// back. Used by the demo and recovery tests.
func (s *Store) WriteUncommitted(stage string, artifact []byte) error {
	sum := sha256.Sum256(artifact)
	if err := s.wal.AppendStage(stage, hex.EncodeToString(sum[:])); err != nil {
		return err
	}
	return os.WriteFile(s.ArtifactPath(stage), artifact, 0o644)
}

// rollbackPending deletes artifacts of stages that were never committed.
func (s *Store) rollbackPending() (int, error) {
	n := 0
	for _, stage := range s.rec.Pending {
		if err := validStage(stage); err != nil {
			return n, err
		}
		for _, p := range []string{s.ArtifactPath(stage), s.ArtifactPath(stage) + ".tmp"} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return n, fmt.Errorf("store: rollback %s: %w", stage, err)
			}
		}
		n++
	}
	s.rec.Pending = nil
	return n, nil
}

// Close closes the underlying WAL.
func (s *Store) Close() error { return s.wal.Close() }
