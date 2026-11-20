// Package checkpoint persists pipeline progress atomically so an interrupted
// run can resume. A checkpoint records the completed stages, the digest of the
// inputs they were computed from and a per-stage resume position. Reuse is
// refused when the input digest changes; that refusal is reported with a
// checkpoint_mismatch fault mapped to exit code 4.
package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/mediamend/mediamend/internal/model"
)

// Version is bumped when the on-disk layout changes incompatibly.
const Version = 1

// FileName is the single checkpoint file inside a run root.
const FileName = "checkpoint.json"

// Stage names in pipeline order.
const (
	StageIngest   = "ingest"
	StageTimeline = "timeline"
	StageGraph    = "graph"
	StageAudio    = "audio"
	StageColor    = "color"
	StageIndex    = "index"
	StageManifest = "manifest"
)

// OrderedStages is the canonical processing order used for deterministic
// skip reporting.
var OrderedStages = []string{
	StageIngest, StageTimeline, StageGraph, StageAudio, StageColor, StageIndex, StageManifest,
}

// Position is an opaque resume marker inside one stage (e.g. last frame seq).
type Position struct {
	TrackID  string `json:"track_id,omitempty"`
	FrameSeq uint64 `json:"frame_seq,omitempty"`
	Extra    string `json:"extra,omitempty"`
}

// Checkpoint is the serialized resume state.
type Checkpoint struct {
	Version     int                 `json:"version"`
	InputDigest string              `json:"input_digest"`
	Completed   map[string]bool     `json:"completed"`
	Positions   map[string]Position `json:"positions"`
	// Metrics persists stage outputs that must survive a resume even when the
	// producing stage is skipped (e.g. seek counts and frame totals).
	Metrics map[string]int64 `json:"metrics,omitempty"`
	// OutputSHA is the stable manifest checksum produced by the manifest stage.
	OutputSHA string    `json:"output_sha,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Metric keys shared with the pipeline; kept here to avoid an import cycle.
const (
	MetricFramesTotal    = "frames_total"
	MetricCorruptLocated = "corrupt_located"
	MetricSeekChecked    = "seek_checked"
)

// Store owns the checkpoint file inside a run directory.
type Store struct {
	path string
}

// NewStore points a store at <root>/checkpoint.json.
func NewStore(root string) *Store {
	return &Store{path: filepath.Join(root, FileName)}
}

// Path exposes the checkpoint location for manifests.
func (s *Store) Path() string { return s.path }

// Fresh creates a checkpoint bound to the given input digest. It does not
// touch disk until the first Save.
func Fresh(inputDigest string) *Checkpoint {
	return &Checkpoint{
		Version:     Version,
		InputDigest: inputDigest,
		Completed:   map[string]bool{},
		Positions:   map[string]Position{},
		Metrics:     map[string]int64{},
	}
}

// SetMetric records a numeric stage output and does not persist by itself;
// the next Save/Mark writes it to disk.
func (cp *Checkpoint) SetMetric(key string, value int64) {
	if cp.Metrics == nil {
		cp.Metrics = map[string]int64{}
	}
	cp.Metrics[key] = value
}

// Metric returns a persisted stage output, or 0 when absent.
func (cp *Checkpoint) Metric(key string) int64 {
	if cp.Metrics == nil {
		return 0
	}
	return cp.Metrics[key]
}

// Save writes the checkpoint atomically: encode to a temp file in the same
// directory, fsync it, then rename over the target so a crash never leaves a
// half-written checkpoint behind.
func (s *Store) Save(cp *Checkpoint) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	cp.UpdatedAt = time.Now().UTC()
	tmp := s.path + ".tmp"
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, s.path)
}

// Load reads and validates a checkpoint. A missing file returns (nil, nil) so
// callers can start a fresh run. A present but unreadable file is an error.
func (s *Store) Load() (*Checkpoint, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("checkpoint corrupt: %w", err)
	}
	if cp.Version != Version {
		return nil, fmt.Errorf("checkpoint version %d unsupported (want %d)", cp.Version, Version)
	}
	return &cp, nil
}

// Resume compares the on-disk checkpoint against the current inputs and
// returns it only when reuse is valid. A digest mismatch yields a
// checkpoint_mismatch fault (exit code 4) explaining the refusal.
func (s *Store) Resume(currentDigest string) (*Checkpoint, []model.Fault, error) {
	cp, err := s.Load()
	if err != nil {
		return nil, nil, err
	}
	if cp == nil {
		return nil, nil, nil
	}
	if cp.InputDigest != currentDigest {
		f := model.Fault{
			Class: model.FaultCheckpoint, Severity: model.SeverityFatal,
			Location: model.SourceLocation{File: s.path},
			Message:  "refusing to reuse checkpoint: input digest changed",
			Expected: currentDigest, Actual: cp.InputDigest,
		}
		return nil, []model.Fault{f}, &model.FaultError{Faults: []model.Fault{f}}
	}
	return cp, nil, nil
}

// Mark records a stage as complete with its final position and persists the
// checkpoint immediately, so a crash after the mark still resumes past it.
func (s *Store) Mark(cp *Checkpoint, stage string, pos Position) error {
	cp.Completed[stage] = true
	cp.Positions[stage] = pos
	return s.Save(cp)
}

// Skip reports whether a stage already completed for matching inputs.
func (cp *Checkpoint) Skip(stage string) bool { return cp.Completed[stage] }

// SkippedStages returns the completed stages in canonical order, ready for
// the resume_skipped_stages manifest field.
func (cp *Checkpoint) SkippedStages() []string {
	var out []string
	for _, st := range OrderedStages {
		if cp.Completed[st] {
			out = append(out, st)
		}
	}
	return out
}

// SortedCompleted is a deterministic helper for logs/tests.
func (cp *Checkpoint) SortedCompleted() []string {
	out := make([]string, 0, len(cp.Completed))
	for st, done := range cp.Completed {
		if done {
			out = append(out, st)
		}
	}
	sort.Strings(out)
	return out
}
