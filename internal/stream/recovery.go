// Package stream implements the bounded executor and its checkpoint recovery.
//
// A checkpoint records the last segment whose output was fully written and
// acknowledged, together with the input digest and the segment size. Resume
// only continues from a checkpoint that matches the current input digest and
// geometry; anything else is discarded and the run starts from segment zero,
// so a resumed run can never splice a stale tail onto new media.
package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ErrCheckpointMismatch reports a checkpoint from different input/geometry.
var ErrCheckpointMismatch = errors.New("stream: checkpoint does not match input")

// Checkpoint is the durable resume point.
type Checkpoint struct {
	Version       int    `json:"version"`
	Digest        string `json:"digest"`
	SegmentSizeUS int64  `json:"segment_size_us"`
	Complete      int    `json:"complete_segments"`
	NextIndex     int    `json:"next_index"`
	LastAckUS     int64  `json:"last_ack_us"`
	// Digests holds the verified per-segment digests so a resumed run can be
	// proven byte-identical to an uninterrupted one.
	Digests []string `json:"digests"`
}

// Validate checks the checkpoint invariants.
func (c *Checkpoint) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("%w: version %d", ErrCheckpointMismatch, c.Version)
	}
	if len(c.Digest) != 64 {
		return fmt.Errorf("%w: digest length %d", ErrCheckpointMismatch, len(c.Digest))
	}
	if c.SegmentSizeUS <= 0 {
		return fmt.Errorf("%w: segment size %d", ErrCheckpointMismatch, c.SegmentSizeUS)
	}
	if c.Complete < 0 || c.NextIndex != c.Complete {
		return fmt.Errorf("%w: complete=%d next=%d", ErrCheckpointMismatch, c.Complete, c.NextIndex)
	}
	if len(c.Digests) > c.Complete {
		return fmt.Errorf("%w: %d digests for %d segments", ErrCheckpointMismatch, len(c.Digests), c.Complete)
	}
	if c.LastAckUS < 0 {
		return fmt.Errorf("%w: negative ack position", ErrCheckpointMismatch)
	}
	return nil
}

// CheckpointStore persists checkpoints atomically.
type CheckpointStore struct {
	Path string
	mu   sync.Mutex
}

// NewCheckpointStore returns a store writing to path.
func NewCheckpointStore(path string) (*CheckpointStore, error) {
	if path == "" {
		return nil, errors.New("stream: empty checkpoint path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("stream: checkpoint dir: %w", err)
	}
	return &CheckpointStore{Path: path}, nil
}

// Load reads the checkpoint, returning ErrCheckpointMismatch when the file is
// absent or unreadable so callers can distinguish "resume" from "start fresh".
func (s *CheckpointStore) Load() (*Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrCheckpointMismatch
		}
		return nil, fmt.Errorf("stream: read checkpoint: %w", err)
	}
	var c Checkpoint
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCheckpointMismatch, err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save writes the checkpoint via temp file + rename.
func (s *CheckpointStore) Save(c *Checkpoint) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("stream: encode checkpoint: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".ckpt-*")
	if err != nil {
		return fmt.Errorf("stream: checkpoint temp: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("stream: checkpoint write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("stream: checkpoint sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("stream: checkpoint close: %w", err)
	}
	return os.Rename(name, s.Path)
}

// Resume builds the execution plan for a run, optionally resuming.
type Resume struct {
	Checkpoint *Checkpoint
	Resumed    bool
	StartIndex int
	StartUS    int64
	Skipped    int
	Reason     string
}

// PlanResume decides where a run starts. The checkpoint is trusted only when
// digest and segment geometry match; otherwise the caller restarts from zero.
func PlanResume(cp *Checkpoint, digest string, segmentSizeUS int64) Resume {
	if cp == nil {
		return Resume{Reason: "no checkpoint"}
	}
	if cp.Digest != digest {
		return Resume{Checkpoint: cp, Reason: "input digest changed"}
	}
	if cp.SegmentSizeUS != segmentSizeUS {
		return Resume{Checkpoint: cp, Reason: "segment size changed"}
	}
	if err := cp.Validate(); err != nil {
		return Resume{Checkpoint: cp, Reason: err.Error()}
	}
	return Resume{
		Checkpoint: cp,
		Resumed:    true,
		StartIndex: cp.NextIndex,
		StartUS:    cp.LastAckUS,
		Skipped:    cp.Complete,
		Reason:     "checkpoint matches input",
	}
}

// Checkpointer advances a checkpoint as segments complete. It is used by the
// pipeline consumer and serializes access so cancellation cannot race a save.
type Checkpointer struct {
	mu     sync.Mutex
	store  *CheckpointStore
	cp     *Checkpoint
	digest string
	sizeUS int64
	saved  int
	// allAck lifts the sequential-completion guard for the verification
	// path: replayed segments may arrive while the run is still at the
	// resume boundary.
	allAck bool
}

// NewCheckpointer builds a checkpointer for a run.
func NewCheckpointer(store *CheckpointStore, digest string, sizeUS int64, resume Resume) *Checkpointer {
	cp := &Checkpoint{Version: 1, Digest: digest, SegmentSizeUS: sizeUS}
	if resume.Resumed && resume.Checkpoint != nil {
		// Copy the checkpoint (including the verified digests) before
		// mutating it; sharing the stored object would let an in-place update
		// hide a digest mismatch.
		c := *resume.Checkpoint
		c.Digests = append([]string(nil), resume.Checkpoint.Digests...)
		cp = &c
	}
	return &Checkpointer{store: store, cp: cp, digest: digest, sizeUS: sizeUS, saved: cp.Complete}
}

// Current returns a copy of the current checkpoint.
func (c *Checkpointer) Current() Checkpoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.cp
}

// Complete records one finished segment. The digest is verified against any
// digest already recorded for that index, catching a resume that produced
// different bytes for previously written segments.
func (c *Checkpointer) Complete(index int, endUS int64, digest string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if index != c.cp.NextIndex && !c.allAck {
		return fmt.Errorf("stream: out-of-order completion %d (want %d)", index, c.cp.NextIndex)
	}
	if len(c.cp.Digests) > index {
		if c.cp.Digests[index] != digest {
			return fmt.Errorf("stream: segment %d digest changed on resume", index)
		}
	} else {
		c.cp.Digests = append(c.cp.Digests, digest)
	}
	if index >= c.cp.Complete {
		c.cp.Complete = index + 1
	}
	if index >= c.cp.NextIndex {
		c.cp.NextIndex = index + 1
	}
	if endUS > c.cp.LastAckUS {
		c.cp.LastAckUS = endUS
	}
	return nil
}

// Flush persists the checkpoint. It is safe to call on cancellation: it writes
// exactly the acknowledged state, never an unconfirmed one.
func (c *Checkpointer) Flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cp.Complete == c.saved {
		return nil
	}
	cp := *c.cp
	cp.Digests = append([]string(nil), c.cp.Digests...)
	if err := c.store.Save(&cp); err != nil {
		return err
	}
	c.saved = c.cp.Complete
	return nil
}

// RunWithRecovery executes jobs with checkpointing and optional resume.
//
// consume is called only for jobs at or after the resume point. On
// cancellation the last acknowledged checkpoint is flushed before returning.
func RunWithRecovery(ctx context.Context, q *Queue, jobs []Job, ck *Checkpointer, resume Resume, consume func(context.Context, *Job) (digest string, err error), opt RunOptions) (Outcome, error) {
	if resume.Resumed {
		if resume.StartIndex > len(jobs) {
			return Outcome{}, fmt.Errorf("stream: checkpoint segment %d beyond %d jobs", resume.StartIndex, len(jobs))
		}
		jobs = jobs[resume.StartIndex:]
	}
	wrapped := func(ctx context.Context, job *Job) error {
		digest, err := consume(ctx, job)
		if err != nil {
			return err
		}
		if err := ck.Complete(job.ID, job.EndUS, digest); err != nil {
			return err
		}
		return ck.Flush()
	}
	out, err := Run(ctx, q, jobs, wrapped, opt)
	flushErr := ck.Flush()
	if err == nil && flushErr != nil {
		err = flushErr
	}
	if resume.Resumed {
		out.Processed += resume.Skipped
		out.Acknowledged += resume.Skipped
	}
	if out.LastAckUS == 0 && resume.Resumed {
		out.LastAckUS = resume.StartUS
	}
	return out, err
}
