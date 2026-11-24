// Package pipeline wires discovery, bounded analysis workers, timeline repair,
// metadata policy, deterministic merge, manifest output and resumable
// checkpoints into one orchestration.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"time"

	"mediad/internal/core"
	"mediad/internal/manifest"
	"mediad/internal/timeline"
)

// CheckpointName is the checkpoint file inside a --checkpoint directory.
const CheckpointName = "checkpoint.json"

// MediaDir is the generated/input media directory relative to a checkpoint/out dir.
const MediaDir = "media"

// Checkpoint states.
const (
	StateActive   = "active"
	StateCanceled = "canceled"
	StateComplete = "complete"
)

// Config is the deterministic processing configuration bound into the
// checkpoint. Scheduling-only controls such as the cancel gate are not part of
// it, so an interrupted run and its resume share one config digest.
type Config struct {
	Workers int `json:"workers"`
	Queue   int `json:"queue"`
}

// DefaultConfig returns the demo defaults.
func DefaultConfig() Config { return Config{Workers: 4, Queue: 8} }

// Normalized fills safe worker/queue bounds (at least one worker and slot).
func (c Config) Normalized() Config {
	if c.Workers <= 0 {
		c.Workers = 4
	}
	if c.Queue <= 0 {
		c.Queue = 8
	}
	if c.Queue < c.Workers {
		// keep at least one result/job slot per worker
		c.Queue = c.Workers
	}
	return c
}

// Digest returns the stable configuration digest.
func (c Config) Digest() string {
	b, _ := json.Marshal(c.Normalized())
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CommittedItem is one fully analyzed, durably committed fragment.
type CommittedItem struct {
	Rel         string               `json:"rel"`
	Obs         timeline.Observation `json:"obs"`
	RoundTripOK bool                 `json:"roundtrip_ok"`
}

// Checkpoint is the resumable state bound to input and config digests.
type Checkpoint struct {
	Version            int                         `json:"version"`
	State              string                      `json:"state"`
	Case               string                      `json:"case,omitempty"`
	Seed               int64                       `json:"seed,omitempty"`
	MediaDir           string                      `json:"media_dir"`
	InputDigest        string                      `json:"input_digest"`
	ConfigDigest       string                      `json:"config_digest"`
	Processing         Config                      `json:"processing"`
	ExpectedResultHash string                      `json:"expected_result_hash,omitempty"`
	ResultHash         string                      `json:"result_hash,omitempty"`
	Committed          []string                    `json:"committed"`
	Items              []CommittedItem             `json:"items"`
	Quarantine         []manifest.QuarantineRecord `json:"quarantine"`
	UpdatedAt          time.Time                   `json:"updated_at"`

	path string
	set  map[string]bool
}

// NewCheckpoint builds an active checkpoint and its ID index.
func NewCheckpoint(path, caseName string, seed int64, inputDigest, configDigest, expectedHash string, clock core.Clock) *Checkpoint {
	if clock == nil {
		clock = core.SystemClock{}
	}
	return &Checkpoint{
		Version: 1, State: StateActive, Case: caseName, Seed: seed,
		MediaDir: MediaDir, InputDigest: inputDigest, ConfigDigest: configDigest,
		ExpectedResultHash: expectedHash, UpdatedAt: clock.Now(),
		path: path, set: map[string]bool{},
	}
}

// LoadCheckpoint reads and validates a checkpoint file.
func LoadCheckpoint(path string) (*Checkpoint, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, &core.InputError{Err: err}
	}
	var cp Checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, &core.InputError{Err: err}
	}
	cp.path = path
	cp.set = map[string]bool{}
	for _, rel := range cp.Committed {
		cp.set[rel] = true
	}
	return &cp, nil
}

// Has reports whether rel was already committed.
func (c *Checkpoint) Has(rel string) bool { return c.set != nil && c.set[rel] }

// AddFragment commits one complete fragment item.
func (c *Checkpoint) AddFragment(item CommittedItem) {
	if c.set == nil {
		c.set = map[string]bool{}
	}
	if c.set[item.Rel] {
		return
	}
	c.set[item.Rel] = true
	c.Committed = append(c.Committed, item.Rel)
	c.Items = append(c.Items, item)
}

// AddQuarantine commits one complete quarantine record.
func (c *Checkpoint) AddQuarantine(rel string, q manifest.QuarantineRecord) {
	if c.set == nil {
		c.set = map[string]bool{}
	}
	if c.set[rel] {
		return
	}
	c.set[rel] = true
	c.Committed = append(c.Committed, rel)
	c.Quarantine = append(c.Quarantine, q)
}

// Save atomically persists the checkpoint.
func (c *Checkpoint) Save(clock core.Clock) error {
	if c.path == "" {
		return nil
	}
	if clock == nil {
		clock = core.SystemClock{}
	}
	c.UpdatedAt = clock.Now()
	return manifest.WriteJSONAtomic(c.path, c)
}
