package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"mediadelivery/internal/fault"
	"mediadelivery/internal/fixture"
	"mediadelivery/internal/index"
	"mediadelivery/internal/manifest"
	"mediadelivery/internal/normalize"
	"mediadelivery/internal/probe"
)

// Runner executes one pipeline and retains its report.
type Runner struct {
	cfg      Config
	recorder *fault.Recorder
	colors   *normalize.ColorRegistry
	store    *index.Store
	cm       *index.CheckpointManager
	dupCount int
	plan     *fixture.Plan
}

// NewRunner validates configuration and prepares index/checkpoint storage.
func NewRunner(cfg Config) (*Runner, error) {
	cfg.applyDefaults()
	if cfg.Cameras < 1 {
		return nil, fmt.Errorf("%w: cameras must be >= 1", ErrInvalidConfig)
	}
	if cfg.WorkDir == "" {
		return nil, fmt.Errorf("%w: work dir required", ErrInvalidConfig)
	}
	store, err := index.NewStore(cfg.WorkDir)
	if err != nil {
		return nil, err
	}
	cm, err := index.NewCheckpointManager(cfg.WorkDir, cfg.RunID)
	if err != nil {
		return nil, err
	}
	return &Runner{
		cfg:      cfg,
		recorder: fault.NewRecorder(),
		colors:   normalize.NewColorRegistry(),
		store:    store,
		cm:       cm,
	}, nil
}

// Run executes the full (or resumed) pipeline and returns metrics.
func (r *Runner) Run() (*Metrics, error) {
	// Load and validate any existing checkpoint; invalid ones are fatal.
	if _, err := r.cm.Load(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCheckpoint, err)
	}

	// Generate (or regenerate deterministically) the synthetic dataset.
	ds, err := fixture.Generate(fixture.Options{
		WorkDir: r.cfg.WorkDir, Format: r.cfg.Format, Cameras: r.cfg.Cameras,
		DurationSec: r.cfg.DurationSec, Profile: r.cfg.Profile,
	})
	if err != nil {
		return nil, err
	}
	r.plan = ds.Plan

	blocks, err := r.buildBlocks(ds)
	if err != nil {
		return nil, err
	}

	// On resume, rehydrate the store from entries persisted in the validated
	// checkpoint so the final index is identical to a single uninterrupted run.
	cp := r.cm.Current()
	for _, e := range cp.Entries {
		r.store.Add(e)
	}

	results, err := r.processBlocks(blocks)
	if err != nil && err != ErrInterrupted {
		return nil, err
	}
	interrupted := err == ErrInterrupted

	// Cross-camera / representation deduplication based on content fingerprints.
	r.deduplicate(results)

	// Persist only the newly processed entries; resumed ones are already in the
	// store via the checkpoint rehydration above.
	for _, res := range results {
		if res.contentHash == "" && res.fileHash == "" {
			continue // skipped/resumed block carries no fresh result
		}
		r.store.Add(res.entry)
	}
	// Flush even on interruption so the partial index is durable and checkable.
	if _, ferr := r.store.Flush(); ferr != nil {
		return nil, fmt.Errorf("%w: %v", ErrIO, ferr)
	}

	if interrupted {
		return nil, ErrInterrupted
	}
	metrics := r.computeMetrics(results)
	return metrics, nil
}

// ReferenceHash runs the same configuration without interruption or resume in
// a throwaway directory and returns its canonical index hash. It provides the
// ground truth for verifying that an interrupted-and-resumed run produced an
// identical index. Generation is deterministic, so the reference is reproducible.
func ReferenceHash(cfg Config) (string, error) {
	ref, err := NewRunner(Config{
		WorkDir:             filepath.Join(cfg.WorkDir, ".reference"),
		Format:              cfg.Format,
		Cameras:             cfg.Cameras,
		DurationSec:         cfg.DurationSec,
		Profile:             cfg.Profile,
		RunID:               cfg.RunID + "-reference",
		MaxWorkers:          cfg.MaxWorkers,
		MaxBlockBytes:       cfg.MaxBlockBytes,
		InterruptAfterBlock: 0,
	})
	if err != nil {
		return "", err
	}
	if _, err := ref.Run(); err != nil {
		return "", err
	}
	return ref.IndexHash(), nil
}

// buildBlocks resolves primary slots from manifests, marks missing/truncated
// files and records the first anomalous position for each fault category.
func (r *Runner) buildBlocks(ds *fixture.Dataset) ([]block, error) {
	var blocks []block
	for _, layout := range ds.Cameras {
		segs, parseErr := r.primarySegments(ds, layout)
		if parseErr != nil {
			// An out-of-order primary playlist is recorded; the well-formed
			// backup manifest still supplies the slot list for recovery.
			r.recorder.AddNoSeq(fault.KindOutOfOrder, layout.Camera, "manifest", true,
				"primary manifest invalid: "+parseErr.Error())
		}
		for _, seg := range segs {
			b := block{
				camera: layout.Camera, role: "primary", sequence: seg.Sequence,
				start90k: seg.Start90k, duration90k: seg.Duration90k,
				path: filepath.Join(ds.Root, layout.Camera, "primary",
					filepath.Base(seg.URI)),
			}
			_, statErr := os.Stat(b.path)
			switch {
			case os.IsNotExist(statErr):
				b.missing = true
				// Recoverability is decided up front from the plan: a slot whose
				// backup is also absent is unrecoverable, the rest are not.
				recoverable := !r.planUnrecoverable(layout.Camera, b.sequence)
				r.recorder.Add(fault.KindMissingSegment, b.camera, "video", b.sequence,
					b.start90k, recoverable, "primary segment file absent")
			case statErr != nil:
				return nil, fmt.Errorf("%w: stat %s: %v", ErrIO, b.path, statErr)
			default:
				if r.isTruncated(b.path, seg) {
					b.truncated = true
					r.recorder.Add(fault.KindTruncatedPacket, b.camera, "video", b.sequence,
						b.start90k, true, "truncated packet detected")
				}
			}
			blocks = append(blocks, b)
		}
	}
	return blocks, nil
}

// primarySegments returns the primary representation's segment list. When the
// primary manifest is invalid (e.g. injected out-of-order sequence), it falls
// back to the always-valid backup manifest so every slot remains addressable.
func (r *Runner) primarySegments(ds *fixture.Dataset, layout fixture.CameraLayout) ([]manifest.Segment, error) {
	if mf, err := manifest.Parse(layout.Camera, r.manifestPath(layout)); err == nil {
		if segs := pickRole(mf, "primary"); segs != nil {
			return segs, nil
		}
	} else {
		// Preserve the parse error for fault recording.
		backupManifestPath := r.backupManifestPath(layout)
		bmf, berr := manifest.Parse(layout.Camera, backupManifestPath)
		if berr != nil {
			return nil, err
		}
		if segs := pickRole(bmf, "backup"); segs != nil {
			return segs, err
		}
		return nil, err
	}
	return nil, fmt.Errorf("%w: no primary representation", ErrManifest)
}

// pickRole extracts segments for a named role, tolerating DASH role naming.
func pickRole(mf *manifest.Manifest, role string) []manifest.Segment {
	for _, rep := range mf.Representations {
		if rep.Role == role {
			return rep.Segments
		}
	}
	// DASH representations are ordered primary then backup; fall back to order.
	idx := 0
	if role == "backup" && len(mf.Representations) > 1 {
		idx = 1
	}
	if idx < len(mf.Representations) {
		return mf.Representations[idx].Segments
	}
	return nil
}

// backupManifestPath returns the on-disk backup playlist for a camera.
func (r *Runner) backupManifestPath(layout fixture.CameraLayout) string {
	if r.cfg.Format == "dash" {
		return layout.DASHMPD
	}
	return filepath.Join(layout.Root, "backup", "index.m3u8")
}

// manifestPath selects HLS master or DASH mpd according to the run format.
func (r *Runner) manifestPath(layout fixture.CameraLayout) string {
	if r.cfg.Format == "dash" {
		return layout.DASHMPD
	}
	return layout.HLSMaster
}

// planUnrecoverable reports whether the fixture plan marks this camera/seq as
// missing from both primary and backup.
func (r *Runner) planUnrecoverable(camera string, seq int) bool {
	if r.plan == nil {
		return false
	}
	return r.plan.IsUnrecoverable(camera, seq)
}

// isTruncated uses probing to confirm a structurally incomplete segment.
func (r *Runner) isTruncated(path string, seg manifest.Segment) bool {
	if seg.Disposition == "" {
		// A file substantially shorter than its declared duration is truncated.
		rep, err := probe.Probe(path)
		if err != nil {
			return true
		}
		expected := seg.Duration90k
		if expected > 0 && rep.Duration90k > 0 && rep.Duration90k*100/expected < 80 {
			return true
		}
		return false
	}
	return true
}

// processBlocks runs bounded-concurrency analysis with recovery and resume.
// When InterruptAfterBlock is set, dispatch stops once that many blocks have
// completed and been checkpointed. In-flight blocks finish durably, then
// ErrInterrupted is returned with the partial results.
func (r *Runner) processBlocks(blocks []block) ([]result, error) {
	sem := make(chan struct{}, r.cfg.MaxWorkers)
	results := make([]result, len(blocks))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	interrupted := false
	completedThisRun := 0

	for i, b := range blocks {
		blockID := fmt.Sprintf("%s/%s/%d", b.camera, b.role, b.sequence)
		if r.cm.IsComplete(blockID) {
			// Resume: this block is already validated and rehydrated from the
			// checkpoint, so leave a zero result and skip reprocessing.
			continue
		}
		// Stop launching new work once enough blocks have completed this run.
		if r.cfg.InterruptAfterBlock > 0 && completedThisRun >= r.cfg.InterruptAfterBlock {
			interrupted = true
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, blk block) {
			defer wg.Done()
			defer func() { <-sem }()
			res, err := r.processOne(blk)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			results[idx] = res
			if err == nil && res.contentHash != "" {
				completedThisRun++
			}
		}(i, b)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if interrupted {
		return results, ErrInterrupted
	}
	return results, nil
}
