package pipeline

import (
	"encoding/json"

	"mediascan/internal/probe"
)

// runStage stores an artefact and records the stage verified. The supplied
// payload function may be nil when the payload was already built.
func (e *runEnv) runStage(stage string, art Artifact, _ func() ([]byte, string, error)) (string, error) {
	e.trace(stage)
	if _, err := e.cache.Store(art); err != nil {
		return "", err
	}
	if err := e.store.Complete(e.cp, stage, StageRecord{InputDigest: e.inputDigest, ParamsDigest: e.params,
		OutputDigest: sha256Hex(art.Payload), RepairDigest: art.ContentDigest}); err != nil {
		return "", err
	}
	return art.ContentDigest, nil
}

func (e *runEnv) runRepair(build func() (repairCache, []byte, error)) (repairCache, error) {
	// Check the content-addressed cache before doing any work; a hit already
	// holds the answer for this exact input and parameter set.
	if pc, ok := e.hydrateRepair(); ok {
		return pc, nil
	}
	pc, payload, err := build()
	if err != nil {
		return pc, err
	}
	if err := e.store.Begin(e.cp, StageRepair, e.inputDigest, e.params); err != nil {
		return pc, err
	}
	if _, err := e.cache.Store(Artifact{Stage: StageRepair, SourceID: artSource, InputDigest: e.inputDigest,
		ParamsDigest: e.params, Payload: payload}); err != nil {
		return pc, err
	}
	rmap := repairMap(e.plan, e.build.timestamps)
	if err := e.store.Complete(e.cp, StageRepair, StageRecord{InputDigest: e.inputDigest, ParamsDigest: e.params,
		OutputDigest: sha256Hex(payload), RepairDigest: rmap.Digest, SegmentsDone: len(e.plan.Segments)}); err != nil {
		return pc, err
	}
	return pc, nil
}

func (e *runEnv) runProbe(build func() (probeCache, []byte, error)) (probeCache, error) {
	if pc, ok := e.hydrateProbe(); ok {
		return pc, nil
	}
	pc, payload, err := build()
	if err != nil {
		return pc, err
	}
	if err := e.store.Begin(e.cp, StageProbe, e.inputDigest, e.params); err != nil {
		return pc, err
	}
	e.trace(StageProbe)
	if _, err := e.cache.Store(Artifact{Stage: StageProbe, SourceID: artSource, InputDigest: e.inputDigest,
		ParamsDigest: e.params, Payload: payload}); err != nil {
		return pc, err
	}
	if err := e.store.Complete(e.cp, StageProbe, StageRecord{InputDigest: e.inputDigest, ParamsDigest: e.params,
		OutputDigest: sha256Hex(payload)}); err != nil {
		return pc, err
	}
	return pc, nil
}

func (e *runEnv) hydrateRepair() (repairCache, bool) {
	a, err := e.cache.Load(StageRepair, artSource, e.inputDigest, e.params, &e.sum.CacheHits)
	if err != nil {
		return repairCache{}, false
	}
	var pc repairCache
	if json.Unmarshal(a.Payload, &pc) != nil {
		return repairCache{}, false
	}
	return pc, true
}

func (e *runEnv) hydrateProbe() (probeCache, bool) {
	a, err := e.cache.Load(StageProbe, artSource, e.inputDigest, e.params, &e.sum.CacheHits)
	if err != nil {
		return probeCache{}, false
	}
	var pc probeCache
	if json.Unmarshal(a.Payload, &pc) != nil {
		return probeCache{}, false
	}
	return pc, true
}

func (e *runEnv) hydrateIndex() (indexCache, bool) {
	a, err := e.cache.Load(StageIndex, artSource, e.inputDigest, e.params, &e.sum.CacheHits)
	if err != nil {
		return indexCache{}, false
	}
	var pc indexCache
	if json.Unmarshal(a.Payload, &pc) != nil {
		return indexCache{}, false
	}
	return pc, true
}

func (e *runEnv) trace(stage string) {
	if e.opt.Trace != nil {
		e.opt.Trace(stage)
	}
}

func (e *runEnv) loadStageHit(stage string) {
	if _, err := e.cache.Load(stage, artSource, e.inputDigest, e.params, &e.sum.CacheHits); err != nil {
		// A missing or invalidated entry for a verified stage is not fatal:
		// the stage will simply be recomputed by the caller's fallback path.
		_ = err
	}
}

// cacheHit probes the cache for an existing artefact, counting a hit. It never
// writes, so a verified checkpoint stage still confirms its files.
func cacheHit(e *runEnv, stage string) bool {
	_, err := e.cache.Load(stage, artSource, e.inputDigest, e.params, &e.sum.CacheHits)
	return err == nil
}

func digestsFor(input, params string) map[string][2]string {
	return map[string][2]string{
		StageManifest: {input, params},
		StageRepair:   {input, params},
		StageProbe:    {input, params},
		StageIndex:    {input, params},
	}
}

func isZeroPolicy(p probe.Policy) bool {
	return p.Channels == "" && p.SampleRate == "" && p.Color == ""
}

func normalizePolicy(p probe.Policy) probe.Policy {
	if isZeroPolicy(p) {
		return probe.DefaultPolicy()
	}
	return p
}

// artSource is the fixed source identity for generated fixtures.
const artSource = "fixture"

type repairCache struct {
	Recovery float64 `json:"recovery"`
	Errors   int     `json:"errors"`
	Digest   string  `json:"digest"`
}

type probeCache struct {
	Sync     int64 `json:"sync"`
	P95      int64 `json:"p95"`
	Findings int   `json:"findings"`
}

type indexCache struct {
	Points int    `json:"points"`
	Valid  bool   `json:"valid"`
	Output string `json:"output"`
	SyncMs int64  `json:"sync_ms"`
}

func runKeyFor(in *inputs, opt Options) string {
	return Key("run", sha256Hex(in.sourceBytes), paramsDigest(opt))
}
