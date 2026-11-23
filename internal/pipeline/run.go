package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mediatimeline/internal/cache"
)

// stageComputers maps stage names to their compute functions. Stage
// implementations register themselves via init (see stages.go), which
// keeps the orchestration decoupled from the stage internals.
var stageComputers = map[string]func(*runCtx) ([]byte, []string, error){}

func registerStage(name string, fn func(*runCtx) ([]byte, []string, error)) {
	stageComputers[name] = fn
}

// LoadParams reads run.json from a run directory.
func LoadParams(dir string) (RunParams, error) {
	var p RunParams
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(data, &p)
}

// LoadMeta reads the fixture manifest from a run directory.
func LoadMeta(dir string) (*FixtureMeta, error) {
	data, err := os.ReadFile(filepath.Join(dir, "inputs", "tracks.json"))
	if err != nil {
		return nil, err
	}
	var m FixtureMeta
	return &m, json.Unmarshal(data, &m)
}

type runCtx struct {
	dir    string
	params RunParams
	meta   *FixtureMeta
	store  *cache.Store
	cpDir  string
	deps   map[string]string // stage -> result digest
	scan   *scanResult
	clock  *clockResult
	comp   *composeResult
	rend   *renderResult
	rep    *Report
}

// Run executes the pipeline over the run directory, resuming from
// valid checkpoints. Only unfinished or invalid stages are recomputed.
func Run(opts Options) (*Report, error) {
	start := time.Now()
	params, err := LoadParams(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("load params: %w", err)
	}
	meta, err := LoadMeta(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("load fixture meta: %w", err)
	}
	c := &runCtx{
		dir: opts.Dir, params: params, meta: meta,
		store: &cache.Store{Dir: filepath.Join(opts.Dir, "cache")},
		cpDir: filepath.Join(opts.Dir, "checkpoints"),
		deps:  map[string]string{},
	}
	rep := &Report{Params: params}
	c.rep = rep
	completed := 0
	for _, stage := range StageOrder {
		info, err := c.runStage(stage)
		if err != nil {
			return rep, err
		}
		rep.Stages = append(rep.Stages, *info)
		if info.CacheHit {
			rep.CacheHits++
			rep.CompletedMicros += info.DurationMicros
		} else {
			rep.RecomputedStages++
		}
		completed++
		if opts.InterruptAfter > 0 && completed >= opts.InterruptAfter {
			rep.Interrupted = true
			break
		}
	}
	if c.scan != nil {
		rep.Corrupt = c.scan.Corrupt
	}
	if c.comp != nil {
		rep.TrackReports = c.comp.Reports
	}
	if c.rend != nil {
		rep.SummarySHA256 = c.rend.Summary
		rep.ManifestDigest = c.rend.ManifestDigest
	}
	rep.RunMicros = time.Since(start).Microseconds()
	return rep, nil
}

// runStage serves one stage from a valid checkpoint+cache entry when
// possible, otherwise recomputes it and records both.
func (c *runCtx) runStage(stage string) (*StageInfo, error) {
	key := c.stageKey(stage)
	if cp, ok := cache.ValidCheckpoint(c.cpDir, stage, key, c.store); ok {
		entry, err := c.store.Get(key)
		if err == nil {
			err = c.store.ValidateSources(entry)
		}
		if err != nil {
			c.rep.Notes = append(c.rep.Notes,
				fmt.Sprintf("cache rejected for stage %s: %v", stage, err))
		} else if err := c.applyPayload(stage, entry.Payload); err != nil {
			c.rep.Notes = append(c.rep.Notes,
				fmt.Sprintf("cache payload invalid for stage %s: %v", stage, err))
		} else {
			c.deps[stage] = entry.ResultDigest
			return &StageInfo{Name: stage, CacheHit: true, DurationMicros: cp.DurationMicros}, nil
		}
	}
	fn, ok := stageComputers[stage]
	if !ok {
		return nil, fmt.Errorf("stage %s not registered", stage)
	}
	t0 := time.Now()
	payload, sources, err := fn(c)
	if err != nil {
		return nil, fmt.Errorf("stage %s: %w", stage, err)
	}
	dur := time.Since(t0).Microseconds()
	digest := cache.ResultDigest(payload)
	digests, err := cache.DigestSources(sources)
	if err != nil {
		return nil, err
	}
	entry := &cache.Entry{
		Key: key, Stage: stage, SourceDigests: digests,
		ResultDigest: digest, Payload: payload,
	}
	if err := c.store.Put(entry); err != nil {
		return nil, err
	}
	if err := cache.WriteCheckpoint(c.cpDir, stage, &cache.Checkpoint{
		Stage: stage, Key: key, ResultDigest: digest, DurationMicros: dur,
	}); err != nil {
		return nil, err
	}
	if err := c.applyPayload(stage, payload); err != nil {
		return nil, err
	}
	c.deps[stage] = digest
	return &StageInfo{Name: stage, Recomputed: true, DurationMicros: dur}, nil
}

// stageKey builds the cache key from the stage, parameters, source
// identifiers and dependency stage digests.
func (c *runCtx) stageKey(stage string) string {
	in := cache.KeyInput{Stage: stage, Params: c.params.Canonical()}
	switch stage {
	case StageScan:
		in.Sources = c.inputFiles()
	case StageClock:
		in.Sources = c.anchorFiles()
		in.Deps = []string{c.deps[StageScan]}
	case StageCompose:
		in.Deps = []string{c.deps[StageClock]}
	case StageRender:
		in.Deps = []string{c.deps[StageCompose]}
	}
	return cache.Key(in)
}

func (c *runCtx) inputFiles() []string {
	var out []string
	for _, t := range c.meta.Tracks {
		out = append(out, filepath.Join(c.dir, "inputs", t.Stream))
		if t.Index != "" {
			out = append(out, filepath.Join(c.dir, "inputs", t.Index))
		}
	}
	return out
}

func (c *runCtx) anchorFiles() []string {
	var out []string
	for _, t := range c.meta.Tracks {
		if t.Anchors == "" {
			continue
		}
		p := filepath.Join(c.dir, "inputs", t.Anchors)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// applyPayload loads a stage payload into the run context.
func (c *runCtx) applyPayload(stage string, payload json.RawMessage) error {
	switch stage {
	case StageScan:
		var r scanResult
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		c.scan = &r
	case StageClock:
		var r clockResult
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		c.clock = &r
	case StageCompose:
		var r composeResult
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		c.comp = &r
	case StageRender:
		var r renderResult
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		c.rend = &r
	}
	return nil
}
