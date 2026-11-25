// Package recovery implements content-hash checkpoints, provenance ledgers and resumable orchestration.
package recovery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"mediad/internal/fault"
)

const CheckpointVersion = 1

// StageOrder is the durable pipeline DAG.
var StageOrder = []string{
	"manifest", "fmp4", "codec", "resample", "loudness",
	"keyframe", "rtp", "ledger", "output",
}

// Params are the user-controlled transformation parameters.
type Params struct {
	Seed      int64   `json:"seed"`
	Rate      int     `json:"rate"`
	Layout    string  `json:"layout"`
	LUFS      float64 `json:"lufs"`
	Damage    string  `json:"damage,omitempty"`
	Interrupt string  `json:"interrupt_after,omitempty"`
}

// SegmentState is the resume point for one source fragment.
type SegmentState struct {
	Index          int               `json:"index"`
	SourceSHA256   string            `json:"source_sha256"`
	InitSHA256     string            `json:"init_sha256,omitempty"`
	Period         int               `json:"period"`
	CompletedStage string            `json:"completed_stage"`
	ArtifactHashes map[string]string `json:"artifact_hashes,omitempty"`
}

// Checkpoint is the durable resume record.
type Checkpoint struct {
	Version        int                      `json:"version"`
	Key            string                   `json:"key"`
	ConfigHash     string                   `json:"config_hash"`
	InputRootHash  string                   `json:"input_root_hash"`
	AdapterVersion string                   `json:"adapter_version"`
	Params         Params                   `json:"params"`
	Stages         map[string]bool          `json:"stages"`
	Segments       map[string]*SegmentState `json:"segments"`
	InterruptedAt  string                   `json:"interrupted_at,omitempty"`
}

// Hashes computes hex SHA-256 of data.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// BuildKey derives the checkpoint key from inputs, parameters and adapter version.
func BuildKey(inputHashes []string, p Params, adapterVersion string) (configHash, rootHash, key string, err error) {
	canonical, err := json.Marshal(struct {
		Params  Params `json:"params"`
		Adapter string `json:"adapter"`
	}{p, adapterVersion})
	if err != nil {
		return "", "", "", fault.New(fault.CheckpointCorrupt, "", -1, "encode params: %v", err)
	}
	configHash = HashBytes(canonical)
	rootCanonical, err := json.Marshal(inputHashes)
	if err != nil {
		return "", "", "", fault.New(fault.CheckpointCorrupt, "", -1, "encode hashes: %v", err)
	}
	rootHash = HashBytes(rootCanonical)
	key = HashBytes([]byte(adapterVersion + "|" + configHash + "|" + rootHash))
	return configHash, rootHash, key, nil
}

// NewCheckpoint creates an empty durable state.
func NewCheckpoint(inputHashes []string, p Params, adapterVersion string) (*Checkpoint, error) {
	cfg, root, key, err := BuildKey(inputHashes, p, adapterVersion)
	if err != nil {
		return nil, err
	}
	return &Checkpoint{
		Version: CheckpointVersion, Key: key, ConfigHash: cfg, InputRootHash: root,
		AdapterVersion: adapterVersion, Params: p,
		Stages: map[string]bool{}, Segments: map[string]*SegmentState{},
	}, nil
}

// LoadCheckpoint reads and versions a checkpoint.
func LoadCheckpoint(path string) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fault.New(fault.IO, path, -1, "checkpoint not found: %v", err)
		}
		return nil, fault.New(fault.IO, path, -1, "read checkpoint: %v", err)
	}
	var c Checkpoint
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fault.New(fault.CheckpointCorrupt, path, 0, "invalid checkpoint JSON: %v", err)
	}
	if c.Version != CheckpointVersion {
		return nil, fault.New(fault.UnknownVersion, path, 0, "unknown checkpoint version %d", c.Version)
	}
	if c.Stages == nil || c.Segments == nil {
		return nil, fault.New(fault.CheckpointCorrupt, path, 0, "checkpoint maps are missing")
	}
	return &c, nil
}

// SaveAtomic writes via a temp file and rename.
func (c *Checkpoint) SaveAtomic(root string) error {
	dir := filepath.Join(root, "work")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fault.New(fault.IO, dir, -1, "create work dir: %v", err)
	}
	path := filepath.Join(dir, "checkpoint.json")
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fault.New(fault.CheckpointCorrupt, path, -1, "encode: %v", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fault.New(fault.IO, tmp, -1, "write temp checkpoint: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fault.New(fault.IO, path, -1, "rename checkpoint: %v", err)
	}
	return nil
}

// MarkStage records a completed stage.
func (c *Checkpoint) MarkStage(stage string) {
	if c.Stages == nil {
		c.Stages = map[string]bool{}
	}
	c.Stages[stage] = true
}

// StageDone reports stage completion.
func (c *Checkpoint) StageDone(stage string) bool {
	return c.Stages != nil && c.Stages[stage]
}

// PutSegment stores or replaces a segment resume point.
func (c *Checkpoint) PutSegment(s *SegmentState) {
	if c.Segments == nil {
		c.Segments = map[string]*SegmentState{}
	}
	c.Segments[s.SourceSHA256] = s
}

// Segment returns state for a source hash.
func (c *Checkpoint) Segment(hash string) (*SegmentState, bool) {
	s, ok := c.Segments[hash]
	return s, ok
}

// InvalidateFrom removes a stage and every downstream stage and segment cursor.
func (c *Checkpoint) InvalidateFrom(stage string) {
	idx := stageIndex(stage)
	if idx < 0 {
		return
	}
	for i := idx; i < len(StageOrder); i++ {
		delete(c.Stages, StageOrder[i])
	}
	for _, s := range c.Segments {
		if stageIndex(s.CompletedStage) >= idx {
			s.CompletedStage = ""
			s.ArtifactHashes = map[string]string{}
		}
	}
}

func stageIndex(stage string) int {
	for i, s := range StageOrder {
		if s == stage {
			return i
		}
	}
	return -1
}
