// Package index persists scan summaries atomically and only ever loads an
// index whose shape is valid and whose source digest matches the container.
package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"medpipe/internal/model"
	"medpipe/internal/scan"
)

// RebuildReason explains why a stored index could not be used.
type RebuildReason string

const (
	ReasonAbsent        RebuildReason = "index_absent"
	ReasonIncomplete    RebuildReason = "index_incomplete_write"
	ReasonDigestChanged RebuildReason = "source_digest_changed"
	ReasonFieldMissing  RebuildReason = "index_field_missing"
	ReasonTempLeft      RebuildReason = "temp_index_left_in_place"
)

// tmpSuffix marks the in-flight file; a file carrying it is never a valid
// index even when the process died mid rename.
const tmpSuffix = ".tmp"

// Store reads and writes index JSON files inside one directory.
type Store struct {
	dir string
}

// NewStore creates the backing directory lazily on first Save.
func NewStore(dir string) *Store { return &Store{dir: dir} }

// Path returns the canonical index location for a source name.
func (s *Store) Path(sourceName string) string {
	return filepath.Join(s.dir, sourceName+".idx.json")
}

// Save writes the index to a temp file, fsyncs it and renames into place.
// Callers therefore never observe a half-written canonical index.
func (s *Store) Save(sourceName string, idx *scan.ScanIndex) error {
	if err := idx.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return model.NewError(model.ErrWrite, "mkdir: %v", err)
	}
	final := s.Path(sourceName)
	tmp := final + tmpSuffix
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return model.NewError(model.ErrWrite, "marshal: %v", err)
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return model.NewError(model.ErrWrite, "open temp: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return model.NewError(model.ErrWrite, "write temp: %v", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return model.NewError(model.ErrWrite, "sync temp: %v", err)
	}
	if err := f.Close(); err != nil {
		return model.NewError(model.ErrWrite, "close temp: %v", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return model.NewError(model.ErrWrite, "rename: %v", err)
	}
	return nil
}

// LoadOrRebuild returns a usable index plus the reason a rebuild was needed
// (empty reason when the stored index was reused). The provided rebuild
// callback runs only when the on-disk copy cannot be trusted.
func (s *Store) LoadOrRebuild(sourceName, sourceDigest string, sourceSize int64,
	rebuild func() (*scan.ScanIndex, error)) (*scan.ScanIndex, RebuildReason, error) {

	idx, reason, err := s.load(sourceName)
	if err == nil {
		if idx.SourceDigest != sourceDigest || idx.SourceSize != sourceSize {
			reason = ReasonDigestChanged
		} else if verr := idx.Validate(); verr != nil {
			reason = ReasonFieldMissing
		} else {
			return idx, "", nil
		}
	}
	fresh, rerr := rebuild()
	if rerr != nil {
		return nil, reason, rerr
	}
	if serr := s.Save(sourceName, fresh); serr != nil {
		return fresh, reason, serr
	}
	return fresh, reason, nil
}

func (s *Store) load(sourceName string) (*scan.ScanIndex, RebuildReason, error) {
	final := s.Path(sourceName)
	data, err := os.ReadFile(final)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A stranded temp file proves a previous write was interrupted.
			if _, terr := os.Stat(final + tmpSuffix); terr == nil {
				return nil, ReasonTempLeft, fmt.Errorf("%w: %s", model.NewError(model.ErrIndexIncomplete, "temp index present"), final+tmpSuffix)
			}
			return nil, ReasonAbsent, err
		}
		return nil, ReasonIncomplete, err
	}
	var idx scan.ScanIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, ReasonIncomplete, model.NewError(model.ErrIndexIncomplete, "json: %v", err)
	}
	if verr := idx.Validate(); verr != nil {
		return nil, ReasonFieldMissing, verr
	}
	return &idx, "", nil
}

// MarkIncomplete simulates an interrupted write by leaving the temp file
// behind without renaming; tests use it to prove such files are never loaded.
func (s *Store) MarkIncomplete(sourceName string, payload []byte) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.Path(sourceName)+tmpSuffix, payload, 0o644)
}
