package pipeline

import (
	"errors"
	"path/filepath"
	"sort"
	"time"

	"mediascan/internal/manifest"
	"mediascan/internal/probe"
)

// ErrInterrupted signals a deliberate stop after the named stage so the caller
// can exercise resumption.
var ErrInterrupted = errors.New("interrupted after stage")

// Options configures one run.
type Options struct {
	Seed           int64
	DropEvery      int
	Damaged        bool
	TruncateTail   int
	InterruptAfter string // "" disables the simulated interruption
	Resume         bool
	CacheDir       string
	DataDir        string
	Policy         probe.Policy
	Now            func() time.Time
	// Trace, when set, is called with the name of every stage that actually
	// executes. Hydrated stages are not traced.
	Trace func(stage string)
}

// Summary is the independently verifiable result of a run.
type Summary struct {
	AVSyncMs        int64
	LossRecoveryPct float64
	P95LatencyMs    int64
	IndexPoints     int
	CacheHits       int
	ErrorSegments   int
	SourceSHA256    string
	OutputSHA256    string
	ManifestSHA256  string
	RepairSHA256    string
	IndexValid      bool
	SourceMatch     bool
	StagesResumed   []string
	Warnings        []string
}

// Run executes the pipeline. It restarts from the beginning of the stage list
// once if a simulated interruption fired, which is what a resumed process
// observes: verified stages are skipped and their committed artefacts reused.
func Run(opt Options) (Summary, error) {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if isZeroPolicy(opt.Policy) {
		opt.Policy = probe.DefaultPolicy()
	}
	opt.Policy = normalizePolicy(opt.Policy)
	build := buildInputs(opt)

	cache, err := OpenCache(opt.CacheDir)
	if err != nil {
		return Summary{}, err
	}
	store, err := OpenCheckpoints(filepath.Join(opt.DataDir, "checkpoints"))
	if err != nil {
		return Summary{}, err
	}
	runKey := runKeyFor(build, opt)
	cp, err := store.Load(runKey)
	if err != nil {
		return Summary{}, err
	}
	if !opt.Resume && opt.InterruptAfter == "" {
		cp = newCheckpoint(runKey)
	}

	params := paramsDigest(opt)
	inputDigest := sha256Hex(append(append([]byte{}, build.sourceBytes...), build.manifestBytes...))
	_, resumed := cp.ResumePoint(digestsFor(inputDigest, params))

	plans, err := manifest.Parse(build.manifestBytes, build.loader, manifest.Options{})
	if err != nil {
		return Summary{}, err
	}
	plan := plans.Plans[0]

	env := &runEnv{
		opt: opt, cache: cache, store: store, cp: cp,
		inputDigest: inputDigest, params: params,
		build: build, plan: plan, plansGroup: plans, sum: &Summary{
			SourceSHA256:   sha256Hex(build.sourceBytes),
			ManifestSHA256: sha256Hex(build.manifestBytes),
		},
	}
	interrupted, err := env.execute()
	if err != nil {
		return *env.sum, err
	}
	env.sum.StagesResumed = resumed

	if interrupted != "" && opt.Resume {
		// A fresh process would reload the checkpoint; emulate that here.
		cp2, err := store.Load(runKey)
		if err != nil {
			return *env.sum, err
		}
		env2 := &runEnv{
			opt: opt, cache: cache, store: store, cp: cp2,
			inputDigest: inputDigest, params: params,
			build: build, plan: plan, plansGroup: plans, sum: &Summary{
				SourceSHA256:   sha256Hex(build.sourceBytes),
				ManifestSHA256: sha256Hex(build.manifestBytes),
			},
		}
		env2.opt.InterruptAfter = ""
		if _, err := env2.execute(); err != nil {
			return *env2.sum, err
		}
		env2.sum.StagesResumed = resumed
		env2.sum.SourceMatch = env2.sum.SourceSHA256 == sha256Hex(build.sourceBytes)
		env2.sum.IndexValid = env2.sum.IndexValid && env2.sum.IndexPoints > 0
		sort.Strings(env2.sum.Warnings)
		return *env2.sum, nil
	}

	env.sum.SourceMatch = env.sum.SourceSHA256 == sha256Hex(build.sourceBytes)
	env.sum.IndexValid = env.sum.IndexValid && env.sum.IndexPoints > 0
	sort.Strings(env.sum.Warnings)
	return *env.sum, nil
}

type runEnv struct {
	opt         Options
	cache       *Cache
	store       *CheckpointStore
	cp          *Checkpoint
	inputDigest string
	params      string
	build       *inputs
	plan        *manifest.Plan
	plansGroup  *manifest.Group
	sum         *Summary
}
