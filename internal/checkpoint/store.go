// Package checkpoint persists per-segment commit records atomically.
// A process interruption can only leave a segment either fully
// committed or absent, never half-written.
package checkpoint

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"archive/internal/fault"
)

// Entry records everything needed to trust a committed segment on a
// later run: the input digest it was derived from, the track mapping
// version, and the output blob digest held by the CAS.
type Entry struct {
	Index          int    `json:"index"`
	URI            string `json:"uri"`
	InputSHA       string `json:"input_sha256"`
	MappingVersion string `json:"mapping_version"`
	OutputSHA      string `json:"output_sha256"`
	OutputBytes    int64  `json:"output_bytes"`
	Tracks         int    `json:"tracks"`
}

// Store is a directory of per-segment entry files.
type Store struct {
	dir string
}

// Open prepares dir for checkpoint files.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fault.Wrap(fault.Fatal, "checkpoint", dir, err)
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(index int) string {
	return filepath.Join(s.dir, fmt.Sprintf("seg-%09d.json", index))
}

// Commit writes e durably: temp file, fsync, rename, fsync directory.
func (s *Store) Commit(e Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return fault.Wrap(fault.Fatal, "checkpoint", e.URI, err)
	}
	tmp, err := os.CreateTemp(s.dir, "commit-*.tmp")
	if err != nil {
		return fault.Wrap(fault.Fatal, "checkpoint", e.URI, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fault.Wrap(fault.Fatal, "checkpoint", e.URI, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fault.Wrap(fault.Fatal, "checkpoint", e.URI, err)
	}
	if err := tmp.Close(); err != nil {
		return fault.Wrap(fault.Fatal, "checkpoint", e.URI, err)
	}
	if err := os.Rename(name, s.path(e.Index)); err != nil {
		return fault.Wrap(fault.Fatal, "checkpoint", e.URI, err)
	}
	return s.syncDir()
}

func (s *Store) syncDir() error {
	d, err := os.Open(s.dir)
	if err != nil {
		return fault.Wrap(fault.Fatal, "checkpoint", s.dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fault.Wrap(fault.Fatal, "checkpoint", s.dir, err)
	}
	return nil
}

// Load returns the entry for index and whether it exists.
func (s *Store) Load(index int) (Entry, bool, error) {
	raw, err := os.ReadFile(s.path(index))
	if os.IsNotExist(err) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fault.Wrap(fault.Fatal, "checkpoint", s.path(index), err)
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return Entry{}, false, fault.Wrap(fault.Fatal, "checkpoint", s.path(index), err)
	}
	if e.Index != index {
		return Entry{}, false, fault.Newf(fault.Fatal, "checkpoint", s.path(index),
			"entry index %d does not match slot %d", e.Index, index)
	}
	if err := e.Validate(); err != nil {
		return Entry{}, false, err
	}
	return e, true, nil
}

// Entries returns all committed entries ordered by index.
func (s *Store) Entries() ([]Entry, error) {
	des, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fault.Wrap(fault.Fatal, "checkpoint", s.dir, err)
	}
	seen := map[int]string{}
	var out []Entry
	for _, de := range des {
		name := de.Name()
		if !strings.HasPrefix(name, "seg-") || !strings.HasSuffix(name, ".json") {
			continue // temp files and foreign files are ignored
		}
		idx, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "seg-"), ".json"))
		if err != nil {
			continue
		}
		e, ok, err := s.Load(idx)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if prev, dup := seen[idx]; dup {
			return nil, fault.Newf(fault.Fatal, "checkpoint", s.dir,
				"duplicate entries for index %d (%s)", idx, prev)
		}
		seen[idx] = name
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

// Validate checks structural invariants of an entry.
func (e Entry) Validate() error {
	if e.Index < 0 {
		return fault.Newf(fault.Fatal, "checkpoint", e.URI, "negative index %d", e.Index)
	}
	if e.URI == "" {
		return fault.Newf(fault.Fatal, "checkpoint", "", "empty URI for index %d", e.Index)
	}
	if e.MappingVersion == "" {
		return fault.Newf(fault.Fatal, "checkpoint", e.URI, "missing mapping version")
	}
	if e.OutputBytes <= 0 {
		return fault.Newf(fault.Fatal, "checkpoint", e.URI, "non-positive output size")
	}
	for _, d := range []struct{ name, val string }{
		{"input", e.InputSHA}, {"output", e.OutputSHA},
	} {
		if len(d.val) != 64 {
			return fault.Newf(fault.Fatal, "checkpoint", e.URI, "malformed %s digest", d.name)
		}
		if _, err := hex.DecodeString(d.val); err != nil {
			return fault.Newf(fault.Fatal, "checkpoint", e.URI, "malformed %s digest", d.name)
		}
	}
	return nil
}

// Remove deletes a committed entry, used when a later verification
// proves the segment must not be presented as complete.
func (s *Store) Remove(index int) error {
	if err := os.Remove(s.path(index)); err != nil && !os.IsNotExist(err) {
		return fault.Wrap(fault.Fatal, "checkpoint", s.path(index), err)
	}
	return s.syncDir()
}
