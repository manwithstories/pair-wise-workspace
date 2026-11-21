// Package recovery persists verifiable stage checkpoints so a pipeline can
// safely resume after corruption or a crashed run, and validates that a
// replayed plan still matches the original input.
package recovery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"mkvarchive/internal/model"
)

// Stage names in fixed pipeline order. Every checkpoint only exists once
// the named stage has fully completed.
const (
	StageParsed   = "parsed"
	StagePlanned  = "planned"
	StageWritten  = "written"
	StageVerified = "verified"
)

var stageOrder = []string{StageParsed, StagePlanned, StageWritten, StageVerified}

// StageCheckpoint records one completed stage with enough evidence to verify
// it before trusting it on a later run.
type StageCheckpoint struct {
	Stage        string `json:"stage"`
	Completed    bool   `json:"completed"`
	BlocksSeen   int    `json:"blocks_seen"`
	InputSHA256  string `json:"input_sha256"`
	PlanSHA256   string `json:"plan_sha256,omitempty"`
	PayloadCount int    `json:"payload_count,omitempty"`
}

// CheckpointFile is the on-disk recovery record.
type CheckpointFile struct {
	SchemaVersion string                     `json:"schema_version"`
	InputPath     string                     `json:"input_path"`
	InputSize     int64                      `json:"input_size"`
	InputSHA256   string                     `json:"input_sha256"`
	Stages        map[string]StageCheckpoint `json:"stages"`
}

// CheckpointSchemaVersion is bumped when the record contract changes.
const CheckpointSchemaVersion = "1.0"

// CheckpointName is the fixed file name inside an output directory.
const CheckpointName = "checkpoint.json"

// Store loads, updates and atomically saves the checkpoint record.
type Store struct {
	dir  string
	file CheckpointFile
}

// NewStore opens an existing checkpoint or starts a fresh record.
func NewStore(dir, inputPath string, inputSize int64, inputSHA []byte) *Store {
	cp := CheckpointFile{
		SchemaVersion: CheckpointSchemaVersion,
		InputPath:     inputPath,
		InputSize:     inputSize,
		InputSHA256:   hex.EncodeToString(inputSHA),
		Stages:        map[string]StageCheckpoint{},
	}
	s := &Store{dir: dir, file: cp}
	if existing, err := s.load(); err == nil {
		s.file = existing
	}
	return s
}

func (s *Store) load() (CheckpointFile, error) {
	var cp CheckpointFile
	raw, err := os.ReadFile(filepath.Join(s.dir, CheckpointName))
	if err != nil {
		return cp, err
	}
	if err = json.Unmarshal(raw, &cp); err != nil {
		return cp, err
	}
	return cp, nil
}

// VerifyInput confirms the current input still matches the recorded one. A
// mismatch means checkpoints cannot be trusted and the run must restart.
func (s *Store) VerifyInput(inputSize int64, inputSHA []byte) error {
	want := hex.EncodeToString(inputSHA)
	if s.file.InputSize != inputSize || s.file.InputSHA256 != want {
		return model.NewError("CHECKPOINT_INPUT_MISMATCH",
			"input identity changed since the checkpoint was written", -1)
	}
	return nil
}

// Mark records a completed stage and persists the file atomically.
func (s *Store) Mark(c StageCheckpoint) error {
	if !isKnownStage(c.Stage) {
		return model.NewError("CHECKPOINT_BAD_STAGE",
			fmt.Sprintf("unknown pipeline stage %q", c.Stage), -1)
	}
	c.Completed = true
	c.InputSHA256 = s.file.InputSHA256
	s.file.Stages[c.Stage] = c
	return s.persist()
}

// LastCompletedStage returns the latest fully completed stage in pipeline
// order, or "" when nothing can be resumed from.
func (s *Store) LastCompletedStage() string {
	last := ""
	for _, stage := range stageOrder {
		if cp, ok := s.file.Stages[stage]; ok && cp.Completed {
			last = stage
		}
	}
	return last
}

// Stage returns the recorded checkpoint for a stage.
func (s *Store) Stage(stage string) (StageCheckpoint, bool) {
	cp, ok := s.file.Stages[stage]
	return cp, ok
}

// File returns a copy of the current record for reporting.
func (s *Store) File() CheckpointFile {
	out := s.file
	out.Stages = map[string]StageCheckpoint{}
	for k, v := range s.file.Stages {
		out.Stages[k] = v
	}
	return out
}

func (s *Store) persist() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s.file, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, CheckpointName+".tmp")
	if err = os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, CheckpointName))
}

// HashPlan returns the SHA-256 of a serialized plan so the planned stage can
// be validated before the write stage resumes.
func HashPlan(planJSON []byte) string {
	sum := sha256.Sum256(planJSON)
	return hex.EncodeToString(sum[:])
}

// ResumeFrom maps the last completed stage to the stage that must run next.
func ResumeFrom(last string) (string, error) {
	if last == "" {
		return StageParsed, nil
	}
	for i, stage := range stageOrder {
		if stage == last {
			if i == len(stageOrder)-1 {
				return "", errors.New("pipeline already complete")
			}
			return stageOrder[i+1], nil
		}
	}
	return "", model.NewError("CHECKPOINT_BAD_STAGE",
		fmt.Sprintf("recorded stage %q is not on the pipeline path", last), -1)
}

func isKnownStage(stage string) bool {
	for _, s := range stageOrder {
		if s == stage {
			return true
		}
	}
	return false
}
