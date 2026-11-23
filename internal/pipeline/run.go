// Package pipeline orchestrates the incremental archive run: parse the
// manifest, read and repair source clocks, index the samples, validate the
// transport trace, verify cached objects and persist checkpoints.
package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"archive.local/archive/internal/cache"
	"archive.local/archive/internal/checkpoint"
	"archive.local/archive/internal/manifest"
	"archive.local/archive/internal/model"
	"archive.local/archive/internal/repair"
)

// Options configures one run.
type Options struct {
	WorkDir      string // explicit working directory for all artefacts
	ManifestPath string
	TracePath    string // optional recorded RTP trace
	Resume       bool
	Duration     time.Duration
	TrackIDs     []uint32 // subset to process; empty = manifest order
}

// TrackStats summarises one repaired track.
type TrackStats struct {
	TrackID      uint32
	Samples      int
	Keyframes    int
	Regressed    int
	Missing      int
	Gaps         int
	FirstUnified int64
	LastUnified  int64
}

// TransportStats summarises RTP validation.
type TransportStats struct {
	Packets       int
	Duplicate     int
	Reordered     int
	BurstLoss     int
	GapRanges     int
	ConfirmedHigh int64
	MaxSeq        int64
	Wraps         int
	// Batches is how many adapted read batches the estimator produced over
	// the trace.
	Batches int
}

// Status is the exit-code-bearing outcome.
type Status int

const (
	// OK means the run completed and every input verified.
	OK Status = iota
	// Damaged means input damage was diagnosed but an index was still built.
	Damaged
	// Failed means an internal error or a resume validation failure.
	Failed
)

// ExitCode maps the run status to the documented exit codes.
func (s Status) ExitCode() int {
	switch s {
	case OK:
		return 0
	case Damaged:
		return 2
	default:
		return 1
	}
}

// Result is the completed or diagnosed run.
type Result struct {
	Status           Status
	Manifest         *manifest.Manifest
	IndexDigest      string
	Tracks           []TrackStats
	Transport        TransportStats
	ResumeUsed       bool
	ResumeMs         int64
	CacheHits        int
	CacheMisses      int
	CacheRejected    int
	Defect           *model.Error
	Recoverable      *model.Recoverable
	DamageOffset     int64
	RecoveredSamples int
	Elapsed          time.Duration
	BlocksDone       int
	BlocksTotal      int
}

// blockSize is the number of keyframe entries per completed index block.
const blockSize = 256

// Run executes one pipeline pass over opts.
func Run(opts Options) (res *Result, retErr error) {
	start := time.Now()
	res = &Result{Status: OK}
	// If we return a nil result on a caller error, still stamp the elapsed
	// time onto whatever result we hand back without dereferencing nil.
	defer func() {
		if res != nil {
			res.Elapsed = time.Since(start)
		}
	}()
	if opts.WorkDir == "" {
		res.Status = Failed
		return res, fmt.Errorf("pipeline: working directory must be explicit")
	}
	absWork, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return nil, err
	}
	if st, serr := os.Stat(absWork); serr != nil || st.IsDir() == false {
		return nil, &model.Error{Code: model.CodeInternal, Sample: -1, OffsetBytes: -1,
			Detail: "working directory does not exist", Err: serr}
	}
	if !filepath.IsAbs(opts.ManifestPath) {
		opts.ManifestPath = filepath.Join(absWork, opts.ManifestPath)
	}
	mf, err := manifest.ParseFileWithRoot(opts.ManifestPath, absWork)
	if err != nil {
		res.Defect = asModelError(err)
		res.Status = Damaged
		return res, err
	}
	res.Manifest = mf
	manifestDigest, err := cache.FileDigest(opts.ManifestPath)
	if err != nil {
		return nil, err
	}
	sourceID := sourceIdentity(mf, manifestDigest)

	tl, err := mf.Timeline()
	if err != nil {
		res.Defect = asModelError(err)
		res.Status = Damaged
		return res, err
	}

	ck, err := checkpoint.NewStore(filepath.Join(absWork, "state"))
	if err != nil {
		return nil, err
	}
	var prior *checkpoint.State
	if opts.Resume {
		prior, err = ck.Load()
		if err != nil {
			res.Defect = asModelError(err)
			res.Status = Failed
			return res, err
		}
	}

	tracks, defects, res2 := readAndRepair(mf, tl, res)
	if res2 != nil {
		res.Defect = asModelError(res2)
		// A concrete input diagnosis is reported as damaged input (exit 2);
		// only a genuine internal failure maps to a failed run (exit 1).
		if res.Defect.Code != model.CodeInternal {
			res.Status = Damaged
		} else {
			res.Status = Failed
		}
		return res, res2
	}
	if res.Defect == nil && len(defects) > 0 {
		res.Defect = defects[0]
	}

	order := mf.TrackIDs()
	if len(opts.TrackIDs) > 0 {
		order = opts.TrackIDs
	}
	idx, err := repair.BuildIndex(tracks, order)
	if err != nil {
		res.Defect = asModelError(err)
		res.Status = Failed
		return res, err
	}
	res.IndexDigest = idx.Digest

	if res.Defect != nil {
		res.Status = Damaged
		if res.Defect.OffsetBytes > 0 {
			res.DamageOffset = res.Defect.OffsetBytes
		}
		if res.Defect.Recoverable != nil {
			res.Recoverable = res.Defect.Recoverable
		}
		res.RecoveredSamples = idx.Samples
	}

	if prior != nil {
		verr := checkpoint.Validate(prior, manifestDigest, sourceID, idx)
		if verr != nil {
			res.Defect = asModelError(verr)
			res.Status = Failed
			return res, verr
		}
		resumeStart := time.Now()
		res.ResumeUsed = true
		res.BlocksDone = len(prior.Completed)
		res.ResumeMs = time.Since(resumeStart).Milliseconds()
	}

	ts, terr := validateTransport(opts.TracePath)
	res.Transport = ts
	if terr != nil {
		res.Defect = asModelError(terr)
		if res.Status == OK {
			res.Status = Damaged
		}
		if res.Defect.OffsetBytes > 0 && res.DamageOffset == 0 {
			res.DamageOffset = res.Defect.OffsetBytes
		}
	}

	stats, cerr := verifyCache(absWork, manifestDigest, order, tracks, idx)
	res.CacheHits, res.CacheMisses, res.CacheRejected = stats.hits, stats.misses, stats.rejected
	if cerr != nil {
		res.Defect = asModelError(cerr)
		if res.Status == OK {
			res.Status = Damaged
		}
	}

	blocks := completedBlocks(prior, idx)
	res.BlocksTotal = len(blockIDs(idx))
	res.BlocksDone = len(blocks)
	if err := ck.Save(checkpoint.State{
		InputDigest:   manifestDigest,
		SourceID:      sourceID,
		ConfirmedHigh: res.Transport.ConfirmedHigh,
		Completed:     blocks,
		IndexDigest:   idx.Digest,
	}); err != nil {
		res.Defect = asModelError(err)
		res.Status = Failed
		return res, err
	}
	return res, nil
}

func asModelError(err error) *model.Error {
	if me, ok := err.(*model.Error); ok {
		return me
	}
	return &model.Error{Code: model.CodeInternal, Sample: -1, OffsetBytes: -1, Detail: err.Error(), Err: err}
}

// readAndRepair parses every declared track and repairs its clock.
