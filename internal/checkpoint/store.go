// Package checkpoint persists scan progress so an interrupted run can resume
// without redoing verified work. Every checkpoint binds the progress cursor
// to a SHA-256 fingerprint of the input: resuming against changed input is
// refused rather than silently mixing old and new results.
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

	"mediascan/internal/fault"
)

// Stage names used in the persisted cursor.
const (
	StageSubtitle = "subtitle"
	StageStream   = "stream"
	StageVerify   = "verify"
	StageDone     = "done"
)

// Segment is one verified input range. Cursors may only rest on these
// boundaries, never inside an unverified region.
type Segment struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Start  int64  `json:"start"`
	End    int64  `json:"end"`
	Stage  string `json:"stage"`
}

// Store is the on-disk checkpoint for one scan directory.
type Store struct {
	// Fingerprint is the hex SHA-256 of every input, keyed by source path.
	Fingerprints map[string]string `json:"fingerprints"`
	// Cursor names the stage and segment boundary last fully verified.
	CursorStage string    `json:"cursor_stage"`
	CursorIndex int       `json:"cursor_index"`
	Segments    []Segment `json:"segments"`
	// SegmentDetails holds full JSON-encoded per-segment reports so a
	// resumed run reconstructs a report byte-identical to a full run.
	SegmentDetails []json.RawMessage `json:"segment_details"`
	// Faults accumulated over every verified segment, in append order. The
	// pipeline re-sorts them deterministically when rendering a report.
	Faults []fault.Fault `json:"faults"`
	// Recovered counts the verified segments that carried recoverable damage.
	Recovered int `json:"recovered"`
	// KeyframeGaps aggregates keyframe gap counts across verified segments.
	KeyframeGaps int `json:"keyframe_gaps"`
	// Params captures run parameters that affect output, so resuming with
	// different options also refuses the stale checkpoint.
	Params string `json:"params"`
}

const fileName = "checkpoint.json"

// FingerprintReader streams a SHA-256 over r without buffering the whole
// input, keeping memory independent of file length.
func FingerprintReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FingerprintFile hashes a file by path.
func FingerprintFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return FingerprintReader(f)
}

// Save atomically writes the store to dir/checkpoint.json via a temp file
// and rename, so a crash never leaves a half-written checkpoint.
func (s *Store) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".checkpoint.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, fileName))
}

// Load reads a checkpoint from dir. A missing file returns (nil, nil) so the
// caller can start a fresh scan.
func Load(dir string) (*Store, error) {
	p := filepath.Join(dir, fileName)
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Store
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("checkpoint unreadable: %w", err)
	}
	return &s, nil
}

// VerifyFingerprint checks every recorded source against its current hash.
// It returns an error naming the first mismatching or missing input, so a
// changed input can never reuse stale progress.
func (s *Store) VerifyFingerprint() error {
	for source, want := range s.Fingerprints {
		got, err := FingerprintFile(source)
		if err != nil {
			return fmt.Errorf("checkpoint input %s unavailable: %w", source, err)
		}
		if got != want {
			return fmt.Errorf("checkpoint refused: %s changed (fingerprint mismatch)", filepath.Base(source))
		}
	}
	return nil
}

// VerifyParams refuses a checkpoint created with different run parameters.
func (s *Store) VerifyParams(params string) error {
	if s.Params != params {
		return fmt.Errorf("checkpoint refused: run parameters differ")
	}
	return nil
}

// AppendSegment records a freshly verified segment and advances the cursor
// to its boundary, then persists atomically. The cursor advances by segment
// order; within the same source the verified range must not move backwards,
// but a new source legitimately starts again at offset zero.
func (s *Store) AppendSegment(dir string, seg Segment) error {
	if seg.End < seg.Start {
		return fmt.Errorf("invalid segment span %d-%d", seg.Start, seg.End)
	}
	for _, prev := range s.Segments {
		if prev.Source == seg.Source && seg.Start < prev.End {
			return fmt.Errorf("cursor would move backwards in %s: %d before verified end %d",
				seg.Source, seg.Start, prev.End)
		}
	}
	s.Segments = append(s.Segments, seg)
	s.CursorStage = seg.Stage
	s.CursorIndex = len(s.Segments)
	return s.Save(dir)
}

// RecordVerified persists one fully verified segment together with its
// complete report payload, faults and aggregates. The cursor is only moved
// after all data for the segment has been appended, so a crash can never
// leave a cursor ahead of the data that justifies it.
func (s *Store) RecordVerified(dir string, seg Segment, detail any, flts []fault.Fault,
	recovered bool, keyframeGaps int) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("encode segment detail: %w", err)
	}
	if err := s.AppendSegment(dir, seg); err != nil {
		return err
	}
	s.SegmentDetails = append(s.SegmentDetails, raw)
	s.Faults = append(s.Faults, flts...)
	if recovered {
		s.Recovered++
	}
	s.KeyframeGaps += keyframeGaps
	return s.Save(dir)
}
