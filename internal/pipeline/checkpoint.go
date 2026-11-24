package pipeline

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
)

// Stage names recorded in the checkpoint. They are ordered as they run.
const (
	StageManifest = "manifest"
	StageRepair   = "repair"
	StageProbe    = "probe"
	StageIndex    = "index"
)

// StageOrder lists the stages in execution order.
var StageOrder = []string{StageManifest, StageRepair, StageIndex, StageProbe}

// StageRecord is the verified state of one stage.
type StageRecord struct {
	Status       string `json:"status"` // pending | running | verified
	InputDigest  string `json:"input_digest,omitempty"`
	ParamsDigest string `json:"params_digest,omitempty"`
	OutputDigest string `json:"output_digest,omitempty"`
	RepairDigest string `json:"repair_digest,omitempty"`
	SegmentsDone int    `json:"segments_done,omitempty"`
	Note         string `json:"note,omitempty"`
}

// Checkpoint is the persisted resume state for one run key.
type Checkpoint struct {
	RunKey string                 `json:"run_key"`
	Stages map[string]StageRecord `json:"stages"`
	LastOK string                 `json:"last_ok,omitempty"`
}

// CheckpointStore persists checkpoints as single JSON files.
type CheckpointStore struct {
	dir string
}

// OpenCheckpoints creates the checkpoint directory.
func OpenCheckpoints(dir string) (*CheckpointStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &CheckpointStore{dir: dir}, nil
}

func (s *CheckpointStore) file(runKey string) string {
	sum := Key("checkpoint", runKey, "")
	return filepath.Join(s.dir, sum+".json")
}

// Load returns the checkpoint for runKey, or an empty one.
func (s *CheckpointStore) Load(runKey string) (*Checkpoint, error) {
	cp := &Checkpoint{RunKey: runKey, Stages: map[string]StageRecord{}}
	data, err := os.ReadFile(s.file(runKey))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cp, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, cp); err != nil {
		// A corrupt checkpoint is discarded, not trusted.
		os.Remove(s.file(runKey))
		return &Checkpoint{RunKey: runKey, Stages: map[string]StageRecord{}}, nil
	}
	if cp.Stages == nil {
		cp.Stages = map[string]StageRecord{}
	}
	return cp, nil
}

// Save writes the checkpoint atomically.
func (s *CheckpointStore) Save(cp *Checkpoint) error {
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.file(cp.RunKey), data)
}

// Verified reports whether a stage completed with the expected inputs.
func (cp *Checkpoint) Verified(stage, inputDigest, paramsDigest string) bool {
	r, ok := cp.Stages[stage]
	if !ok {
		return false
	}
	return r.Status == "verified" && r.InputDigest == inputDigest && r.ParamsDigest == paramsDigest
}

// Begin records a stage as running. It is persisted before the work starts so
// an interruption leaves an explicit "running" marker.
func (s *CheckpointStore) Begin(cp *Checkpoint, stage, inputDigest, paramsDigest string) error {
	cp.Stages[stage] = StageRecord{Status: "running", InputDigest: inputDigest, ParamsDigest: paramsDigest}
	return s.Save(cp)
}

// Complete records a stage as verified with its output digest.
func (s *CheckpointStore) Complete(cp *Checkpoint, stage string, rec StageRecord) error {
	rec.Status = "verified"
	cp.Stages[stage] = rec
	cp.LastOK = stage
	return s.Save(cp)
}

// ResumePoint returns the first stage that must run, plus whether everything
// before it was verified. A "running" stage is never skipped.
func (cp *Checkpoint) ResumePoint(stageDigests map[string][2]string) (string, []string) {
	var done []string
	for _, stage := range StageOrder {
		want, ok := stageDigests[stage]
		if !ok {
			continue
		}
		if !cp.Verified(stage, want[0], want[1]) {
			return stage, done
		}
		done = append(done, stage)
	}
	return "", done
}

// CompletedStages returns verified stage names in order.
func (cp *Checkpoint) CompletedStages() []string {
	var out []string
	for _, stage := range StageOrder {
		if cp.Stages[stage].Status == "verified" {
			out = append(out, stage)
		}
	}
	sort.Strings(out)
	return out
}
