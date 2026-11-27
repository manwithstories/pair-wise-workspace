// Package index persists the block-level analysis results to a local,
// deterministic index and manages checkpoints that advance only after the
// output has been written, validated and fsynced. All writes stay inside the
// configured working directory; inputs are never modified.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Entry is one indexed block (a resolved segment slot on the shared timeline).
type Entry struct {
	Camera       string `json:"camera"`
	Representation string `json:"representation"`
	Sequence     int    `json:"sequence"`
	Start90k     int64  `json:"start_90k"`
	Duration90k  int64  `json:"duration_90k"`
	// SourcePath is the segment file actually used (primary or recovered backup).
	SourcePath string `json:"source_path"`
	// ContentHash is over the decoded-content fingerprint, stable across
	// re-encodes of identical content.
	ContentHash string `json:"content_hash"`
	// FileHash is over the on-disk segment bytes.
	FileHash string `json:"file_hash"`
	// SilenceIntervals lists detected silence ranges in 90 kHz ticks.
	SilenceIntervals []SilenceSpan `json:"silence_intervals,omitempty"`
	// Recovered marks slots filled from a backup representation.
	Recovered bool `json:"recovered"`
	// DuplicateOf points at the first entry with equivalent content, if any.
	DuplicateOf string `json:"duplicate_of,omitempty"`
}

// SilenceSpan is a half-open silence interval on the shared clock.
type SilenceSpan struct {
	Start90k int64 `json:"start_90k"`
	End90k   int64 `json:"end_90k"`
}

// Store is the deterministic block index for a run.
type Store struct {
	root    string
	entries []Entry
}

// NewStore creates (or opens) an index rooted at workDir/index.
func NewStore(workDir string) (*Store, error) {
	root := filepath.Join(workDir, "index")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("index: mkdir: %w", err)
	}
	return &Store{root: root}, nil
}

// Root returns the index directory.
func (s *Store) Root() string { return s.root }

// Add appends an entry. Callers add blocks in deterministic camera/sequence
// order; the store sorts on flush so the persisted record is canonical.
func (s *Store) Add(e Entry) {
	s.entries = append(s.entries, e)
}

// Entries returns the current entries.
func (s *Store) Entries() []Entry {
	out := make([]Entry, len(s.entries))
	copy(out, s.entries)
	return out
}

// canonicalOrder sorts entries deterministically by camera, sequence, role.
func canonicalOrder(es []Entry) {
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].Camera != es[j].Camera {
			return es[i].Camera < es[j].Camera
		}
		if es[i].Sequence != es[j].Sequence {
			return es[i].Sequence < es[j].Sequence
		}
		return es[i].Representation < es[j].Representation
	})
}

// Flush writes the index atomically: data is written to a temp file, fsynced,
// then renamed over the target and the directory is fsynced so the record is
// durable before any checkpoint can advance.
func (s *Store) Flush() (string, error) {
	ordered := make([]Entry, len(s.entries))
	copy(ordered, s.entries)
	canonicalOrder(ordered)

	doc := struct {
		Version string  `json:"version"`
		Count   int     `json:"count"`
		Entries []Entry `json:"entries"`
	}{Version: "1", Count: len(ordered), Entries: ordered}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("index: marshal: %w", err)
	}
	data = append(data, '\n')
	target := filepath.Join(s.root, "index.json")
	if err := atomicWrite(target, data); err != nil {
		return "", err
	}
	return target, nil
}

// Hash returns a stable hash over the canonical entry set. Two identical runs
// produce the same hash even if entries were added in different orders.
func (s *Store) Hash() string {
	ordered := make([]Entry, len(s.entries))
	copy(ordered, s.entries)
	canonicalOrder(ordered)
	data, err := json.Marshal(ordered)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// atomicWrite writes data to a temp file in the same directory, fsyncs it,
// renames into place and fsyncs the directory.
func atomicWrite(target string, data []byte) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("index: temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("index: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("index: fsync data: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("index: close: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		cleanup()
		return fmt.Errorf("index: rename: %w", err)
	}
	// fsync the directory so the rename itself is durable.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// HashBytes returns the hex SHA-256 of raw bytes, used for file hashing.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
