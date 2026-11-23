// Package checkpoint persists pipeline progress atomically so a run can resume
// after cancellation, a disk failure or process restart.
//
// A record stores each input track's byte cursor, the last fully completed
// segment and the manifest hash accumulated so far. Writes go to a temp file
// in the same directory and are renamed into place, so a crash can only leave
// the previous complete record or the new one — never a half-written file.
// On resume the committed output directory is re-verified: complete segments
// are skipped, a partial final segment is detected and safely redone.
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mediad/diag"
)

// Record is the serializable progress state.
type Record struct {
	// Version allows future format migrations to be detected.
	Version int `json:"version"`
	// Cursors maps track ID to the count of fully consumed input bytes.
	Cursors map[string]int64 `json:"cursors"`
	// LastSegment is the index of the last fully completed segment, or -1.
	LastSegment int `json:"last_segment"`
	// SegmentHashes maps "rep#index" to the hex sha256 of that rep's segment
	// file, so every bitrate layer's output is verified independently.
	SegmentHashes map[string]string `json:"segment_hashes"`
	// ManifestHash is the hash of the manifest produced so far.
	ManifestHash string `json:"manifest_hash"`
}

// Store manages one checkpoint file inside an output directory.
type Store struct {
	path string
	dir  string
}

const fileName = "checkpoint.json"

// NewStore opens (or prepares) a checkpoint store under dir.
func NewStore(dir string) *Store {
	return &Store{dir: dir, path: filepath.Join(dir, fileName)}
}

// Save atomically replaces the checkpoint record.
func (s *Store) Save(rec Record) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return diag.New(diag.EOutput, "", "create checkpoint dir: %v", err)
	}
	rec.Version = 1
	buf, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return diag.New(diag.EOutput, "", "encode checkpoint: %v", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return diag.New(diag.EOutput, "", "write checkpoint temp: %v", err)
	}
	// fsync before rename so the bytes are durable even on power loss.
	if f, err := os.Open(tmp); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return diag.New(diag.EOutput, "", "rename checkpoint: %v", err)
	}
	return nil
}

// Load reads and validates the current record. ErrMissing means no checkpoint
// exists yet (a fresh run), not a failure.
func (s *Store) Load() (Record, error) {
	buf, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, ErrMissing
	}
	if err != nil {
		return Record{}, diag.New(diag.EOutput, "", "read checkpoint: %v", err)
	}
	var rec Record
	if err := json.Unmarshal(buf, &rec); err != nil {
		return Record{}, diag.New(diag.EResume, "", "checkpoint is corrupt: %v", err)
	}
	if rec.Version != 1 {
		return Record{}, diag.New(diag.EResume, "", "unsupported checkpoint version %d", rec.Version)
	}
	return rec, nil
}

// ErrMissing is returned by Load when no checkpoint has been saved yet.
var ErrMissing = errors.New("checkpoint: no record present")

// Clear removes the checkpoint (used after a successful full verification).
func (s *Store) Clear() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return diag.New(diag.EOutput, "", "remove checkpoint: %v", err)
	}
	return nil
}

// SegmentPath returns the canonical output path of one segment file.
func SegmentPath(dir, rep string, idx int) string {
	return filepath.Join(dir, "segments", fmt.Sprintf("%s-%05d.seg", rep, idx))
}

// HashFile returns the hex sha256 of a file's bytes.
func HashFile(path string) (string, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return "", diag.New(diag.EOutput, "", "hash %s: %v", path, err)
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

// VerifyCommitted re-checks every segment listed in the record against its
// stored hash. It returns the index of the first segment that is missing or
// mismatched in any representation (the redo point), or -1 when every
// committed segment file is intact.
func VerifyCommitted(dir string, rec Record) (redoFrom int, err error) {
	redoFrom = -1
	for key, want := range rec.SegmentHashes {
		rep, idx, perr := parseSegKey(key)
		if perr != nil {
			return -1, diag.New(diag.EResume, "", "bad segment hash key %q", key)
		}
		if idx > rec.LastSegment {
			continue
		}
		path := SegmentPath(dir, rep, idx)
		got, ferr := HashFile(path)
		if ferr != nil || got != want {
			if redoFrom < 0 || idx < redoFrom {
				redoFrom = idx
			}
		}
	}
	return redoFrom, nil
}

// SegmentKey namespaces a segment hash entry by representation and index.
func SegmentKey(rep string, idx int) string { return rep + "#" + strconv.Itoa(idx) }

// segKey kept as an unexported alias for internal call sites.
func segKey(rep string, idx int) string { return SegmentKey(rep, idx) }

// parseSegKey splits a "rep#index" checkpoint key.
func parseSegKey(key string) (string, int, error) {
	pos := strings.LastIndexByte(key, '#')
	if pos <= 0 || pos == len(key)-1 {
		return "", 0, errBadKey
	}
	idx, err := strconv.Atoi(key[pos+1:])
	if err != nil {
		return "", 0, errBadKey
	}
	return key[:pos], idx, nil
}

var errBadKey = diag.New(diag.EResume, "", "malformed segment key")
