package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"

	"relayaudit/internal/fault"
)

// Commit atomically persists a validated staged-output prefix together with
// the checkpoint that describes it. data must be exactly the prefix the state
// claims; the prefix digest in st is recomputed and stored by the caller's
// pipeline, and Commit refuses any mismatch so a stale pair can never be
// committed.
func (s *Store) Commit(st State, data []byte) error {
	if int64(len(data)) != st.EmittedBytes {
		return fault.New(fault.Checkpoint, 0,
			"staged prefix is %d bytes but state claims %d", len(data), st.EmittedBytes)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != st.PrefixSHA256 {
		return fault.New(fault.Checkpoint, 0, "staged prefix digest does not match state")
	}
	if err := s.writeAtomic(s.StagingPath(), data); err != nil {
		return err
	}
	return s.Save(st)
}

// CommitAppend persists an extended prefix by appending only the bytes past
// the previous committed length. The caller guarantees that data is the
// previous prefix with chunk appended; the full digest is verified here.
func (s *Store) CommitAppend(st State, data, chunk []byte) error {
	if int64(len(data)) != st.EmittedBytes {
		return fault.New(fault.Checkpoint, 0,
			"staged prefix is %d bytes but state claims %d", len(data), st.EmittedBytes)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != st.PrefixSHA256 {
		return fault.New(fault.Checkpoint, 0, "staged prefix digest does not match state")
	}
	if err := s.StageChunk(int64(len(data))-int64(len(chunk)), chunk); err != nil {
		return err
	}
	return s.Save(st)
}

// StageChunk extends the staged-output file to off+len(chunk) by writing
// chunk at off, truncating any stale bytes beyond off first. It is the
// incremental counterpart of Commit: a long run appends only new bytes per
// checkpoint instead of rewriting the whole prefix. The file is synced before
// returning so a crash cannot leave a torn tail.
func (s *Store) StageChunk(off int64, chunk []byte) error {
	if off < 0 {
		return fault.New(fault.Checkpoint, 0, "negative staging offset %d", off)
	}
	f, err := os.OpenFile(s.StagingPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fault.Wrap(fault.Checkpoint, off, err, "open staging: %v", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fault.Wrap(fault.Checkpoint, off, err, "stat staging: %v", err)
	}
	if info.Size() > off {
		if err := f.Truncate(off); err != nil {
			return fault.Wrap(fault.Checkpoint, off, err, "truncate stale staging tail: %v", err)
		}
	} else if info.Size() < off {
		return fault.New(fault.Checkpoint, off,
			"staging file is %d bytes, cannot append at %d", info.Size(), off)
	}
	if _, err := f.WriteAt(chunk, off); err != nil {
		return fault.Wrap(fault.Checkpoint, off, err, "append staging: %v", err)
	}
	if len(chunk) == 0 {
		// A truncation alone still needs to reach disk.
		if err := f.Truncate(off); err != nil {
			return fault.Wrap(fault.Checkpoint, off, err, "truncate staging: %v", err)
		}
	}
	if err := f.Sync(); err != nil {
		return fault.Wrap(fault.Checkpoint, off, err, "sync staging: %v", err)
	}
	return nil
}

// ReadStaging returns the persisted staged-output prefix.
func (s *Store) ReadStaging() ([]byte, error) {
	body, err := os.ReadFile(s.StagingPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fault.Wrap(fault.Checkpoint, 0, err, "read staging: %v", err)
	}
	return body, nil
}

// PrefixDigest returns the hex SHA-256 of data.
func PrefixDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
