// Package checkpoint persists resume state atomically and verifies it
// against the source input and already-written outputs. A checkpoint
// records the input fingerprint, parser offset, committed watermark and
// output digest; a resume is refused when any of these no longer match.
package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"mediapack/internal/clock"
)

// FileName is the fixed checkpoint file name inside a working directory.
const FileName = "checkpoint.json"

// State is the serializable resume record.
type State struct {
	// Version allows future on-disk format evolution.
	Version int `json:"version"`
	// SourceFP is the SHA-256 fingerprint of the input file.
	SourceFP string `json:"source_fingerprint"`
	// ParseOffset is the next byte the parser should read.
	ParseOffset int64 `json:"parse_offset"`
	// WatermarkNS is the last committed timeline watermark.
	WatermarkNS int64 `json:"watermark_ns"`
	// ManifestSHA is the digest of the manifest written so far.
	ManifestSHA string `json:"manifest_sha"`
	// SegmentCount is the number of committed segments.
	SegmentCount int `json:"segment_count"`
	// QueueBytes is the charged queue occupancy at checkpoint time.
	QueueBytes int64 `json:"queue_bytes"`
}

// CurrentVersion is the supported on-disk format version.
const CurrentVersion = 1

// Store reads and writes checkpoint files within one working directory.
type Store struct {
	dir string
}

// NewStore creates a store rooted at dir (created if needed).
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, clock.NewError(clock.CatCheckpoint, "checkpoint dir must not be empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, clock.NewError(clock.CatCheckpoint, "create dir: %v", err)
	}
	return &Store{dir: dir}, nil
}

// Path returns the absolute checkpoint file path.
func (s *Store) Path() string { return filepath.Join(s.dir, FileName) }

// Exists reports whether a checkpoint file is present.
func (s *Store) Exists() bool {
	_, err := os.Stat(s.Path())
	return err == nil
}

// Save atomically writes state: encode to a temp file in the same
// directory, fsync, then rename over the target so a crash can never
// leave a half-written checkpoint behind.
func (s *Store) Save(st State) error {
	st.Version = CurrentVersion
	if st.SourceFP == "" {
		return clock.NewError(clock.CatCheckpoint, "refusing to save checkpoint without source fingerprint")
	}
	raw, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		return clock.NewError(clock.CatCheckpoint, "encode: %v", err)
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(s.dir, ".checkpoint-*.tmp")
	if err != nil {
		return clock.NewError(clock.CatCheckpoint, "temp file: %v", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		cleanup()
		return clock.NewError(clock.CatCheckpoint, "write temp: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return clock.NewError(clock.CatCheckpoint, "fsync temp: %v", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return clock.NewError(clock.CatCheckpoint, "close temp: %v", err)
	}
	if err := os.Rename(tmpName, s.Path()); err != nil {
		cleanup()
		return clock.NewError(clock.CatCheckpoint, "atomic rename: %v", err)
	}
	return nil
}

// Load reads and validates the checkpoint structure.
func (s *Store) Load() (State, error) {
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, clock.NewError(clock.CatCheckpoint, "no checkpoint at %s", s.Path())
		}
		return State{}, clock.NewError(clock.CatCheckpoint, "read: %v", err)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, clock.NewError(clock.CatCheckpoint, "corrupt checkpoint json: %v", err)
	}
	if st.Version != CurrentVersion {
		return State{}, clock.NewError(clock.CatCheckpoint,
			"unsupported checkpoint version %d (want %d)", st.Version, CurrentVersion)
	}
	if st.ParseOffset < 0 || st.WatermarkNS < 0 || st.SourceFP == "" {
		return State{}, clock.NewError(clock.CatCheckpoint, "inconsistent checkpoint fields: %+v", st)
	}
	return st, nil
}

// Mismatch describes exactly why a resume was refused. It wraps a
// clock.MediaError carrying CatCheckpoint so errors.As keeps working.
type Mismatch struct {
	Field  string
	Expect string
	Got    string
}

func (m *Mismatch) Error() string {
	return fmt.Sprintf("checkpoint mismatch on %s: expected %q, got %q", m.Field, m.Expect, m.Got)
}

// Unwrap exposes the underlying CatCheckpoint MediaError.
func (m *Mismatch) Unwrap() error {
	return clock.NewError(clock.CatCheckpoint, m.Error())
}

// Verify checks a loaded state against the current source fingerprint
// and the existing manifest digest. A mismatch is returned as a
// *Mismatch (category CatCheckpoint) so the caller can locate the cause.
func (s *Store) Verify(st State, sourceFP, manifestSHA string) error {
	if st.SourceFP != sourceFP {
		return &Mismatch{Field: "source_fingerprint", Expect: st.SourceFP, Got: sourceFP}
	}
	if st.ManifestSHA != "" && st.ManifestSHA != manifestSHA {
		return &Mismatch{Field: "manifest_sha256", Expect: st.ManifestSHA, Got: manifestSHA}
	}
	return nil
}// Remove deletes the checkpoint (used after a successful full run).
func (s *Store) Remove() error {
	if err := os.Remove(s.Path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return clock.NewError(clock.CatCheckpoint, "remove: %v", err)
	}
	return nil
}
