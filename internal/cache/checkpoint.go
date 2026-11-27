package cache

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"avcheck/internal/fault"
)

// Checkpoint records one pipeline stage's output and the identities it
// depends on. Digest covers every other field, so any tampering is detected.
type Checkpoint struct {
	Stage        string `json:"stage"`
	SourceID     string `json:"source_id"`     // logical identity of the input
	SourceDigest string `json:"source_digest"` // digest of the input bytes
	ParamDigest  string `json:"param_digest"`  // digest of stage parameters
	PrevDigest   string `json:"prev_digest"`   // digest of the previous stage's checkpoint
	BlobID       string `json:"blob_id"`       // CAS id of the stage output
	Digest       string `json:"digest"`
}

// ComputeDigest derives the checkpoint self-digest.
func (c *Checkpoint) ComputeDigest() string {
	body, _ := json.Marshal(struct {
		S, SI, SD, PD, PV, B string
	}{c.Stage, c.SourceID, c.SourceDigest, c.ParamDigest, c.PrevDigest, c.BlobID})
	return DigestOf(body)
}

// Chain manages ordered stage checkpoints in Dir backed by a blob Store.
type Chain struct {
	Dir    string
	Store  *Store
	Stages []string // pipeline order
}

func (ch *Chain) path(stage string) string {
	return filepath.Join(ch.Dir, "checkpoint-"+stage+".json")
}

// Save persists a checkpoint atomically after verifying that PrevDigest
// matches the on-disk checkpoint of the previous stage.
func (ch *Chain) Save(c *Checkpoint) error {
	const op = "cache.checkpoint"
	if idx := indexOf(ch.Stages, c.Stage); idx > 0 {
		prev, err := ch.Load(ch.Stages[idx-1])
		if err != nil {
			return fault.New(fault.Corrupt, op, int64(idx),
				"cannot verify predecessor: "+err.Error())
		}
		if prev.Digest != c.PrevDigest {
			return fault.New(fault.Corrupt, op, int64(idx), "predecessor digest mismatch")
		}
	}
	c.Digest = c.ComputeDigest()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ch.Dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(ch.Dir, ".tmp-ck-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), ch.path(c.Stage))
}

// Load reads a checkpoint and verifies its self-digest.
func (ch *Chain) Load(stage string) (*Checkpoint, error) {
	data, err := os.ReadFile(ch.path(stage))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fault.New(fault.Corrupt, "cache.checkpoint", 0, "no checkpoint "+stage)
		}
		return nil, err
	}
	var c Checkpoint
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fault.New(fault.Corrupt, "cache.checkpoint", 0, "unparseable checkpoint "+stage)
	}
	if c.Digest != c.ComputeDigest() {
		return nil, fault.New(fault.Corrupt, "cache.checkpoint", 0, "checkpoint digest mismatch "+stage)
	}
	return &c, nil
}

// Valid reports whether the checkpoint for stage exists, verifies, and still
// matches the given source identity, source digest and parameter digest. Any
// mismatch invalidates this stage and every later stage so the pipeline
// resumes from the earliest affected stage.
func (ch *Chain) Valid(stage, sourceID, sourceDigest, paramDigest string) (*Checkpoint, bool) {
	c, err := ch.Load(stage)
	if err != nil {
		return nil, false
	}
	if c.SourceID != sourceID || c.SourceDigest != sourceDigest || c.ParamDigest != paramDigest {
		ch.InvalidateFrom(stage)
		return nil, false
	}
	if !ch.Store.Has(c.BlobID) {
		ch.InvalidateFrom(stage)
		return nil, false
	}
	return c, true
}

// InvalidateFrom removes the checkpoints of stage and all later stages.
func (ch *Chain) InvalidateFrom(stage string) {
	for i := indexOf(ch.Stages, stage); i >= 0 && i < len(ch.Stages); i++ {
		os.Remove(ch.path(ch.Stages[i]))
	}
}

func indexOf(stages []string, s string) int {
	for i, v := range stages {
		if v == s {
			return i
		}
	}
	return -1
}
