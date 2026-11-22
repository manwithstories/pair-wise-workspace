package batch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"avscan/internal/model"
)

// BlockResult is the deterministic summary of one processed sample block.
type BlockResult struct {
	BlockIndex  int   `json:"block_index"`
	TrackID     uint32 `json:"track_id"`
	SampleStart int   `json:"sample_start"`
	SampleEnd   int   `json:"sample_end"`
	KeyFrames   int   `json:"key_frames"`
	FaultCount  int   `json:"fault_count"`
	Damaged     bool  `json:"damaged"`
}

// IndexRecord is the full on-disk index. Its JSON bytes feed index_sha256.
type IndexRecord struct {
	InputDigest    string             `json:"input_digest"`
	BlockSize      int                `json:"block_size"`
	Blocks         []BlockResult      `json:"blocks"`
	DamageSegments []model.DamageSegment `json:"damage_segments"`
	Faults         []model.Fault      `json:"faults"`
}

// AtomicWriteJSON serializes v to path via a temp file and rename so a crash
// never leaves a half-written index.
func AtomicWriteJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: %v", model.ErrIndexWrite, err)
	}
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("%w: %v", model.ErrIndexWrite, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("%w: %v", model.ErrIndexWrite, err)
	}
	return nil
}

// IndexSHA256 returns the hex SHA-256 of the canonical JSON of an index.
func IndexSHA256(rec IndexRecord) (string, error) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// BlockProcessor is the per-block callback supplied by the application.
type BlockProcessor func(trackID uint32, start, end int) (BlockResult, error)

// RunOptions configures a bounded block run and its resume behaviour.
type RunOptions struct {
	IndexPath string
	BlockSize int
	Resume    bool
	Summary   InputSummary
}

// Runner executes blocks track by track. Only the current block is held in
// memory; progress is checkpointed after each successful block.
type Runner struct {
	opts    RunOptions
	cp      Checkpoint
	blocks  []BlockResult
	resumed bool
	// globalIndex counts block positions across all tracks, including blocks
	// skipped during a resume because they were already committed.
	globalIndex int
}

// NewRunner validates resume state. Without --resume any prior checkpoint is
// ignored and processing restarts from block zero.
func NewRunner(opts RunOptions) (*Runner, error) {
	if opts.BlockSize <= 0 {
		return nil, fmt.Errorf("%w: block size must be positive", model.ErrInvalidArgs)
	}
	digest, err := opts.Summary.Digest()
	if err != nil {
		return nil, err
	}
	r := &Runner{
		opts: opts,
		cp:   Checkpoint{InputDigest: digest, BlockSize: opts.BlockSize},
	}
	if opts.Resume {
		loaded, err := LoadCheckpoint(opts.IndexPath, opts.Summary)
		if err != nil {
			return nil, err
		}
		if loaded != nil {
			r.cp = *loaded
			// globalIndex always numbers blocks from zero across both runs;
			// the checkpoint only tells us which prefix is already committed.
			r.resumed = true
		}
	}
	return r, nil
}

// Process runs the callback over every block of one track. A block that
// returns an error is recorded as damaged and the run continues with the next
// block, matching the "one bad block does not stop the scan" requirement.
func (r *Runner) Process(trackID uint32, sampleCount int, fn BlockProcessor) error {
	for start := 0; start < sampleCount; start += r.opts.BlockSize {
		blockIdx := r.globalIndex
		r.globalIndex++
		// Resume only re-runs blocks committed before the interruption to
		// reconstruct their deterministic results; it does not move the
		// checkpoint forward. Blocks at or past Completed are genuinely new.
		replay := r.resumed && blockIdx < r.cp.Completed
		end := start + r.opts.BlockSize
		if end > sampleCount {
			end = sampleCount
		}
		res, err := fn(trackID, start, end)
		if err != nil && !replay {
			res = BlockResult{
				BlockIndex: blockIdx, TrackID: trackID,
				SampleStart: start, SampleEnd: end, Damaged: true,
				FaultCount: 1,
			}
			err = nil
		}
		if err != nil {
			return err
		}
		res.BlockIndex = blockIdx
		r.blocks = append(r.blocks, res)
		if replay {
			continue
		}
		if err := r.commit(res); err != nil {
			return err
		}
	}
	return nil
}

// commit advances the chained result digest and moves the checkpoint forward.
// If the checkpoint write fails, the previous valid checkpoint stays on disk
// because SaveCheckpoint uses a temp file and rename.
func (r *Runner) commit(res BlockResult) error {
	raw := fmt.Sprintf("%d|%d|%d|%d|%d|%t",
		res.BlockIndex, res.TrackID, res.SampleStart,
		res.SampleEnd, res.FaultCount, res.Damaged)
	r.cp.ResultDigest = chainHash(r.cp.ResultDigest, raw)
	r.cp.Completed++
	r.cp.TotalSamples += res.SampleEnd - res.SampleStart
	return SaveCheckpoint(r.opts.IndexPath, r.cp)
}

// Blocks returns the results accumulated in this run.
func (r *Runner) Blocks() []BlockResult { return r.blocks }

// Completed returns the number of committed blocks. On resume this starts at
// the checkpoint value and only grows when genuinely new blocks commit.
func (r *Runner) Completed() int { return r.cp.Completed }

// Resumed reports whether an existing checkpoint was accepted.
func (r *Runner) Resumed() bool { return r.resumed }
