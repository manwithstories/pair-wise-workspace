// Package checkpoint persists scan progress to a local JSON file so a run
// interrupted by cancellation can resume without recomputing finished
// segments. Each save is atomic (temp file + rename), and a resume is
// rejected unless both the input digest and parameter digest match.
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Segment records one fully completed, already-digested work range.
type Segment struct {
	ID    int64 `json:"id"`
	Start int64 `json:"start"`
	End   int64 `json:"end"` // exclusive
	// PartialDigest covers this segment's canonical result bytes, so the
	// final report digest can be recomputed identically after resume.
	PartialDigest string `json:"partial_digest"`
}

// Store is the on-disk checkpoint payload.
type Store struct {
	Version      int       `json:"version"`
	InputDigest  string    `json:"input_digest"`
	ParamsDigest string    `json:"params_digest"`
	Segments     []Segment `json:"segments"`
	// FrameCount tracks total recomputed frames after a resume.
	FrameCount int64 `json:"frame_count"`
}

// CurrentVersion is the checkpoint schema version.
const CurrentVersion = 1

var (
	// ErrMismatch means input or parameter digests differ; resume is unsafe.
	ErrMismatch = errors.New("checkpoint: input or parameter digest mismatch")
	// ErrMalformed means the checkpoint file cannot be parsed.
	ErrMalformed = errors.New("checkpoint: malformed checkpoint file")
)

// DigestBytes returns the hex SHA-256 of raw input.
func DigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// DigestReader streams a SHA-256 without loading the whole input twice.
func DigestReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DigestParams produces a stable digest from canonical parameter key/values.
// Callers pass already-serialized canonical bytes so ordering is controlled.
func DigestParams(canonicalParams []byte) string {
	sum := sha256.Sum256(canonicalParams)
	return hex.EncodeToString(sum[:])
}

// New initializes an empty store bound to specific input and parameters.
func New(inputDigest, paramsDigest string) *Store {
	return &Store{
		Version:      CurrentVersion,
		InputDigest:  inputDigest,
		ParamsDigest: paramsDigest,
		Segments:     []Segment{},
	}
}

// Load reads and validates a checkpoint against the current run's digests.
// It returns ErrMismatch when either digest differs so the caller must start
// a fresh run rather than silently mixing incompatible results.
func Load(path, inputDigest, paramsDigest string) (*Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Store
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if s.Version != CurrentVersion {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrMalformed, s.Version)
	}
	if s.InputDigest != inputDigest || s.ParamsDigest != paramsDigest {
		return nil, fmt.Errorf("%w: stored input=%s params=%s",
			ErrMismatch, s.InputDigest, s.ParamsDigest)
	}
	if s.Segments == nil {
		s.Segments = []Segment{}
	}
	return &s, nil
}

// Save atomically writes the store: data goes to a sibling temp file and is
// renamed over the target, so an interrupted process never leaves a partial
// checkpoint that a later resume could trust.
func (s *Store) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}

// Append records a completed segment in ascending ID order and persists it.
func (s *Store) Append(seg Segment, path string) error {
	if len(s.Segments) > 0 && seg.ID <= s.Segments[len(s.Segments)-1].ID {
		return fmt.Errorf("checkpoint: segment %d out of order", seg.ID)
	}
	s.Segments = append(s.Segments, seg)
	s.FrameCount += seg.End - seg.Start
	return s.Save(path)
}

// CompletedUpTo returns the highest contiguous end offset finished so far.
func (s *Store) CompletedUpTo() int64 {
	var upTo int64
	for _, seg := range s.Segments {
		if seg.Start == upTo {
			upTo = seg.End
		}
	}
	return upTo
}

// IsSegmentDone reports whether a segment ID was already completed.
func (s *Store) IsSegmentDone(id int64) bool {
	for _, seg := range s.Segments {
		if seg.ID == id {
			return true
		}
	}
	return false
}
