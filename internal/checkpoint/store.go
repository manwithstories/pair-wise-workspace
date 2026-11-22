// Package checkpoint persists batched progress so a interrupted run can
// resume without reprocessing completed chunks. Writes are atomic, the input
// manifest digest is recorded, and a changed input is rejected rather than
// silently mixing old and new results.
package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mediad/internal/fault"
	segmenthash "mediad/internal/hash"
)

// FormatVersion is bumped when the on-disk shape changes incompatibly.
const FormatVersion = 1

// ManifestDigestSize is the expected hex length of a manifest digest.
const ManifestDigestSize = 64

// Chunk records the completion of a contiguous block of segments.
type Chunk struct {
	Index    int                  `json:"index"`
	Segments []segmenthash.Record `json:"segments"`
	Faults   []fault.Fault        `json:"faults,omitempty"`
}

// State is the serialised checkpoint.
type State struct {
	Version        int     `json:"version"`
	ManifestSHA256 string  `json:"manifest_sha256"`
	ChunkSize      int     `json:"chunk_size"`
	Chunks         []Chunk `json:"chunks"`
	// Faults at the job level, for example input or manifest problems.
	Faults []fault.Fault `json:"faults,omitempty"`
}

// ErrInputChanged means the checkpoint belongs to a different input.
var ErrInputChanged = errors.New("checkpoint: input manifest changed")

// ErrCorrupt means the checkpoint cannot be trusted.
var ErrCorrupt = errors.New("checkpoint: corrupt checkpoint")

// Store reads and writes checkpoints inside one directory.
type Store struct {
	Dir string
	// fileName is relative to Dir; it must stay within Dir.
	fileName string
}

// New creates a Store rooted at dir. The directory is created on first save.
func New(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("checkpoint: empty directory")
	}
	return &Store{Dir: dir, fileName: "checkpoint.json"}, nil
}

// Path returns the checkpoint file location.
func (s *Store) Path() string { return filepath.Join(s.Dir, s.fileName) }

// Validate rejects a chunk plan that cannot be represented.
func Validate(chunkSize int) error {
	if chunkSize <= 0 {
		return fmt.Errorf("checkpoint: chunk size %d must be positive", chunkSize)
	}
	if chunkSize > 100_000 {
		return fmt.Errorf("checkpoint: chunk size %d is unbounded", chunkSize)
	}
	return nil
}

// ChunkCount returns the number of chunks needed for n segments.
func ChunkCount(n, chunkSize int) int {
	if n <= 0 || chunkSize <= 0 {
		return 0
	}
	return (n + chunkSize - 1) / chunkSize
}

// ChunkRange returns the [start,end) segment indices for a chunk.
func ChunkRange(chunk, n, chunkSize int) (int, int) {
	if chunk < 0 || chunkSize <= 0 || n <= 0 {
		return 0, 0
	}
	start := chunk * chunkSize
	if start >= n {
		return n, n
	}
	end := start + chunkSize
	if end > n {
		end = n
	}
	return start, end
}

// NewState builds an empty checkpoint bound to a manifest digest.
func NewState(manifestDigest string, chunkSize int) (*State, error) {
	if len(manifestDigest) != ManifestDigestSize || !isLowerHex(manifestDigest) {
		return nil, fmt.Errorf("checkpoint: invalid manifest digest %q", manifestDigest)
	}
	if err := Validate(chunkSize); err != nil {
		return nil, err
	}
	return &State{Version: FormatVersion, ManifestSHA256: manifestDigest, ChunkSize: chunkSize}, nil
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return len(s) > 0
}

// Save writes the state atomically via a temporary file and rename.
func (s *Store) Save(st *State) error {
	if st == nil {
		return errors.New("checkpoint: nil state")
	}
	if st.Version != FormatVersion {
		return fmt.Errorf("%w: version %d", ErrCorrupt, st.Version)
	}
	if len(st.ManifestSHA256) != ManifestDigestSize {
		return fmt.Errorf("%w: manifest digest", ErrCorrupt)
	}
	if err := Validate(st.ChunkSize); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return fmt.Errorf("checkpoint: create dir: %w", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("checkpoint: marshal: %w", err)
	}
	tmp, err := os.CreateTemp(s.Dir, ".checkpoint-*.tmp")
	if err != nil {
		return fmt.Errorf("checkpoint: temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("checkpoint: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("checkpoint: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("checkpoint: close: %w", err)
	}
	if err := os.Rename(tmpName, s.Path()); err != nil {
		return fmt.Errorf("checkpoint: rename: %w", err)
	}
	return nil
}

// Load reads a checkpoint. A missing file returns (nil, nil) so callers can
// distinguish "first run" from an error.
func (s *Store) Load() (*State, error) {
	data, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint: read: %w", err)
	}
	var st State
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if st.Version != FormatVersion {
		return nil, fmt.Errorf("%w: version %d", ErrCorrupt, st.Version)
	}
	if err := Validate(st.ChunkSize); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if len(st.ManifestSHA256) != ManifestDigestSize || !isLowerHex(st.ManifestSHA256) {
		return nil, fmt.Errorf("%w: manifest digest", ErrCorrupt)
	}
	seen := map[int]bool{}
	for _, c := range st.Chunks {
		if c.Index < 0 || seen[c.Index] {
			return nil, fmt.Errorf("%w: duplicate or negative chunk %d", ErrCorrupt, c.Index)
		}
		seen[c.Index] = true
		for _, rec := range c.Segments {
			if err := rec.Validate(); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
			}
		}
	}
	return &st, nil
}

// Resume checks that a loaded state matches the current input, then returns
// the completed chunk indexes. A changed digest yields ErrInputChanged and
// no completed chunks.
func Resume(st *State, manifestDigest string) ([]int, error) {
	if st == nil {
		return nil, nil
	}
	if st.ManifestSHA256 != manifestDigest {
		return nil, fmt.Errorf("%w: checkpoint %s, input %s", ErrInputChanged, st.ManifestSHA256, manifestDigest)
	}
	done := make([]int, 0, len(st.Chunks))
	for _, c := range st.Chunks {
		done = append(done, c.Index)
	}
	sort.Ints(done)
	return done, nil
}

// AddChunk records a completed chunk, replacing any earlier copy. Chunks must
// be added in a deterministic order by the caller.
func (st *State) AddChunk(c Chunk) error {
	if c.Index < 0 {
		return fmt.Errorf("checkpoint: negative chunk index %d", c.Index)
	}
	for _, rec := range c.Segments {
		if err := rec.Validate(); err != nil {
			return err
		}
	}
	for i := range st.Chunks {
		if st.Chunks[i].Index == c.Index {
			st.Chunks[i] = c
			return nil
		}
	}
	st.Chunks = append(st.Chunks, c)
	sort.SliceStable(st.Chunks, func(i, j int) bool { return st.Chunks[i].Index < st.Chunks[j].Index })
	return nil
}

// CompletedSegments counts all segment records across completed chunks.
func (st *State) CompletedSegments() int {
	n := 0
	for _, c := range st.Chunks {
		n += len(c.Segments)
	}
	return n
}

// AllSegments returns completed segment records in chunk order.
func (st *State) AllSegments() []segmenthash.Record {
	var out []segmenthash.Record
	for _, c := range st.Chunks {
		out = append(out, c.Segments...)
	}
	return out
}

// AllFaults returns chunk faults in chunk order.
func (st *State) AllFaults() []fault.Fault {
	var out []fault.Fault
	for _, c := range st.Chunks {
		out = append(out, c.Faults...)
	}
	return out
}
