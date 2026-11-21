// Package recover manages idempotent checkpoints so an interrupted run can
// resume from the last fully processed segment and a repeated run can prove
// it produced the same conclusions.
package recover

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mediascan/internal/index"
	"mediascan/internal/model"
)

// Version of the checkpoint layout.
const Version = 1

// ErrInputChanged is returned when a checkpoint was bound to different inputs.
var ErrInputChanged = errors.New("recover: input digest mismatch")

// Params binds analysis knobs into the checkpoint so a parameter change also
// invalidates prior results.
type Params struct {
	LossEvery      int   `json:"loss_every"`
	InterruptAfter int   `json:"interrupt_after"`
	AVToleranceMS  int64 `json:"av_tolerance_ms"`
	MaxGOPMS       int64 `json:"max_gop_ms"`
	ReorderWindow  int   `json:"reorder_window"`
}

// Normalize fills defaults and rejects nonsense.
func (p Params) Normalize() (Params, error) {
	if p.LossEvery <= 1 {
		return p, fmt.Errorf("recover: loss_every must be > 1, got %d", p.LossEvery)
	}
	if p.InterruptAfter < 0 {
		return p, fmt.Errorf("recover: interrupt_after must be >= 0")
	}
	if p.AVToleranceMS <= 0 {
		p.AVToleranceMS = 40
	}
	if p.MaxGOPMS <= 0 {
		p.MaxGOPMS = 2000
	}
	if p.ReorderWindow <= 0 {
		p.ReorderWindow = 64
	}
	return p, nil
}

// Checkpoint is the durable resume record. ResultSHA is the digest of the
// conclusions produced by the run that wrote it; a repeated run over unchanged
// inputs must reproduce it exactly.
type Checkpoint struct {
	Version      int             `json:"version"`
	ManifestSHA  string          `json:"manifest_sha256"` // digest of normalized timeline
	SegmentSHAs  string          `json:"segment_set_sha256"`
	Params       Params          `json:"params"`
	IndexVersion int             `json:"index_version"`
	Completed    int             `json:"completed_segments"`
	NextSeg      int             `json:"next_segment"`
	Status       string          `json:"status"` // "running" | "interrupted" | "complete"
	UpdatedAt    string          `json:"updated_at"`
	Results      json.RawMessage `json:"results,omitempty"`
	ResultSHA    string          `json:"result_sha256,omitempty"`
}

// Store reads and writes checkpoints under a work directory.
type Store struct {
	Path string
}

// NewStore returns the checkpoint path for a work dir.
func NewStore(work string) *Store {
	return &Store{Path: filepath.Join(work, "checkpoint.json")}
}

// Save writes the checkpoint atomically.
func (s *Store) Save(cp *Checkpoint) error {
	cp.Version = Version
	cp.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return index.WriteJSON(s.Path, cp)
}

// Load reads the checkpoint. A missing file returns (nil, nil).
func (s *Store) Load() (*Checkpoint, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("recover: corrupt checkpoint %s: %w", s.Path, err)
	}
	if cp.Version != Version {
		return nil, fmt.Errorf("recover: checkpoint version %d, want %d", cp.Version, Version)
	}
	return &cp, nil
}

// Binding describes the inputs a checkpoint is valid for.
type Binding struct {
	ManifestSHA  string
	SegmentSHAs  string
	Params       Params
	IndexVersion int
}

// BindingOf computes the binding from an index and parameters.
func BindingOf(idx *index.Index, params Params) Binding {
	return Binding{
		ManifestSHA:  idx.InputSHA,
		SegmentSHAs:  segmentSetDigest(idx),
		Params:       params,
		IndexVersion: idx.Version,
	}
}

// Validate reports whether the checkpoint may be reused. A nil checkpoint or
// any mismatch returns ErrInputChanged wrapped with the field that drifted.
func (cp *Checkpoint) Validate(b Binding) error {
	if cp == nil {
		return fmt.Errorf("%w: no checkpoint", ErrInputChanged)
	}
	switch {
	case cp.ManifestSHA != b.ManifestSHA:
		return fmt.Errorf("%w: manifest %s != %s", ErrInputChanged, short(cp.ManifestSHA), short(b.ManifestSHA))
	case cp.SegmentSHAs != b.SegmentSHAs:
		return fmt.Errorf("%w: segment set changed", ErrInputChanged)
	case cp.Params != b.Params:
		return fmt.Errorf("%w: analysis parameters changed", ErrInputChanged)
	case cp.IndexVersion != b.IndexVersion:
		return fmt.Errorf("%w: index version %d != %d", ErrInputChanged, cp.IndexVersion, b.IndexVersion)
	}
	return nil
}

// segmentSetDigest binds the checkpoint to exactly the set of segment digests
// that were present, so adding or removing media invalidates prior results.
func segmentSetDigest(idx *index.Index) string {
	h := digestWriter{}
	fmt.Fprintf(&h, "index=%d|entries=%d\n", idx.Version, len(idx.Entries))
	for _, e := range idx.Entries {
		fmt.Fprintf(&h, "%d|%s|%s|%d|%s|%s\n", e.Ordinal, e.URI, e.SHA256, e.Size, e.Start, e.Duration)
	}
	return h.Sum()
}

// CompletedEntries returns entries[:NextSeg] that were actually hashed,
// stopping at the first gap so resume never skips a missing dependency.
func CompletedEntries(cp *Checkpoint, idx *index.Index) []index.Entry {
	if cp == nil || idx == nil {
		return nil
	}
	n := cp.NextSeg
	if n > len(idx.Entries) {
		n = len(idx.Entries)
	}
	out := idx.Entries[:n]
	for i, e := range out {
		if !e.Present {
			return out[:i]
		}
	}
	return out
}

// SimulateInterrupt is the demo hook: it marks a checkpoint as interrupted
// after n segments so the next run exercises resume.
func SimulateInterrupt(cp *Checkpoint, n int) {
	if n < 0 {
		n = 0
	}
	cp.NextSeg = n
	cp.Completed = n
	cp.Status = "interrupted"
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "..."
	}
	return s
}

// FaultsFromResults decodes the stored result payload for provenance checks.
func FaultsFromResults(raw json.RawMessage) ([]model.Fault, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var fs []model.Fault
	if err := json.Unmarshal(raw, &fs); err != nil {
		return nil, err
	}
	return fs, nil
}
