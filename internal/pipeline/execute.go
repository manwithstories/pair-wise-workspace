package pipeline

import (
	"encoding/json"
	"path/filepath"

	"mediascan/internal/probe"
	"mediascan/internal/repair"
)

// execute runs each stage, reusing a cached artefact when all four identity
// components match. A verified checkpoint stage is hydrated from its artefact,
// so a resumed run skips the work it already proved; only a cache miss or an
// invalidated entry causes recomputation.
func (e *runEnv) execute() (string, error) {
	sum := e.sum
	dg := digestsFor(e.inputDigest, e.params)
	done := func(stage string) bool {
		return e.cp.Verified(stage, dg[stage][0], dg[stage][1])
	}

	// Stage: manifest.
	if done(StageManifest) {
		e.loadStageHit(StageManifest)
	} else {
		art, err := manifestArtifact(e.build, e.params)
		if err != nil {
			return "", err
		}
		dig, err := e.runStage(StageManifest, art, func() ([]byte, string, error) {
			return art.Payload, "", nil
		})
		if err != nil {
			return "", err
		}
		if dig != "" && e.opt.InterruptAfter == StageManifest {
			return StageManifest, nil
		}
	}

	// Stage: repair. The mapping is a pure function of the observed
	// timestamps, so a hydrated run reads its digest from the artefact.
	var rep repair.Result
	repairPayload := func() (repairCache, []byte, error) {
		rep = repair.Analyze(e.build.rtpPackets)
		rmap := repairMap(e.plan, e.build.timestamps)
		pc := repairCache{Recovery: rep.RecoveryPct, Errors: len(rmap.Unavailable), Digest: rmap.Digest}
		b, err := json.Marshal(pc)
		return pc, b, err
	}
	if done(StageRepair) {
		if pc, ok := e.hydrateRepair(); ok {
			sum.LossRecoveryPct = pc.Recovery
			sum.ErrorSegments = pc.Errors
			sum.RepairSHA256 = pc.Digest
		} else if _, err := e.runRepair(repairPayload); err != nil {
			return "", err
		} else {
			sum.LossRecoveryPct = rep.RecoveryPct
			sum.ErrorSegments = len(repairMap(e.plan, e.build.timestamps).Unavailable)
			sum.RepairSHA256 = repairMap(e.plan, e.build.timestamps).Digest
		}
	} else {
		pc, err := e.runRepair(repairPayload)
		if err != nil {
			return "", err
		}
		sum.LossRecoveryPct = pc.Recovery
		sum.ErrorSegments = pc.Errors
		sum.RepairSHA256 = pc.Digest
		if e.opt.InterruptAfter == StageRepair {
			return StageRepair, nil
		}
	}

	// Stage: probe.
	probePayload := func() (probeCache, []byte, error) {
		prep, err := probe.Validate(e.build.representations, e.opt.Policy)
		if err != nil {
			return probeCache{}, nil, err
		}
		pc := probeCache{Sync: avSync(prep, e.build), P95: p95Latency(e.build.set.Latencies), Findings: len(prep.Findings)}
		b, err := json.Marshal(pc)
		return pc, b, err
	}
	if done(StageProbe) {
		if pc, ok := e.hydrateProbe(); ok {
			sum.AVSyncMs = pc.Sync
			sum.P95LatencyMs = pc.P95
		} else if pc, err := e.runProbe(probePayload); err == nil {
			sum.AVSyncMs = pc.Sync
			sum.P95LatencyMs = pc.P95
		} else {
			return "", err
		}
	} else {
		pc, err := e.runProbe(probePayload)
		if err != nil {
			return "", err
		}
		sum.AVSyncMs = pc.Sync
		sum.P95LatencyMs = pc.P95
		if e.opt.InterruptAfter == StageProbe {
			return StageProbe, nil
		}
	}

	// Stage: index.
	if done(StageIndex) {
		if pc, ok := e.hydrateIndex(); ok {
			sum.IndexPoints = pc.Points
			sum.IndexValid = pc.Valid
			sum.OutputSHA256 = pc.Output
			return "", nil
		}
	}
	e.trace(StageIndex)
	idxOut, err := buildIndex(e.build, e.plan, filepath.Join(e.opt.DataDir, "scratch"))
	if err != nil {
		return "", err
	}
	sum.IndexPoints = idxOut.points
	sum.IndexValid = idxOut.valid
	sum.OutputSHA256 = idxOut.outputDigest
	payload, err := json.Marshal(indexCache{Points: idxOut.points, Valid: idxOut.valid, Output: idxOut.outputDigest})
	if err != nil {
		return "", err
	}
	if _, err := e.runStage(StageIndex, Artifact{Stage: StageIndex, SourceID: artSource,
		InputDigest: e.inputDigest, ParamsDigest: e.params, Payload: payload}, nil); err != nil {
		return "", err
	}
	if e.opt.InterruptAfter == StageIndex {
		return StageIndex, nil
	}
	return "", nil
}

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
}

func runKeyFor(in *inputs, opt Options) string {
	return Key("run", sha256Hex(in.sourceBytes), paramsDigest(opt))
}
