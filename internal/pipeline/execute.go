package pipeline

import (
	"encoding/json"
	"path/filepath"

	"mediascan/internal/probe"
	"mediascan/internal/repair"
)

// execute runs each stage, reusing a cached artefact when all four identity
// components match and recomputing otherwise. A verified checkpoint stage is
// never re-executed, which is what makes an interrupted run resumable.
func (e *runEnv) execute() (string, error) {
	sum := e.sum
	dg := digestsFor(e.inputDigest, e.params)
	done := func(stage string) bool {
		return e.cp.Verified(stage, dg[stage][0], dg[stage][1])
	}

	// Stage: manifest.
	if !done(StageManifest) {
		if err := e.store.Begin(e.cp, StageManifest, e.inputDigest, e.params); err != nil {
			return "", err
		}
		art, err := manifestArtifact(e.build, e.params)
		if err != nil {
			return "", err
		}
		dig, err := e.cache.Store(art)
		if err != nil {
			return "", err
		}
		if err := e.store.Complete(e.cp, StageManifest, StageRecord{
			InputDigest: e.inputDigest, ParamsDigest: e.params, OutputDigest: dig,
		}); err != nil {
			return "", err
		}
		if e.opt.InterruptAfter == StageManifest {
			return StageManifest, nil
		}
	} else if _, err := e.cache.Load(StageManifest, artSource, e.inputDigest, e.params, &e.sum.CacheHits); err == nil {
		// Identity re-checked by Load; count the hit.
	}

	// The repair stage produces the mapping the index reconciles against.
	rmap := repairMap(e.plan, e.build.timestamps)
	rep := repair.Analyze(e.build.rtpPackets)
	sum.LossRecoveryPct = rep.RecoveryPct
	sum.RepairSHA256 = rmap.Digest
	sum.ErrorSegments = len(rmap.Unavailable)
	sum.Warnings = append(sum.Warnings, rep.Warnings...)
	sum.Warnings = append(sum.Warnings, rmap.Warnings...)

	if !done(StageRepair) {
		payload, err := json.Marshal(repairCache{Recovery: rep.RecoveryPct, Errors: sum.ErrorSegments, Digest: rmap.Digest})
		if err != nil {
			return "", err
		}
		if !cacheHit(e, StageRepair) {
			if err := e.store.Begin(e.cp, StageRepair, e.inputDigest, e.params); err != nil {
				return "", err
			}
			if _, err := e.cache.Store(Artifact{Stage: StageRepair, SourceID: artSource, InputDigest: e.inputDigest,
				ParamsDigest: e.params, Payload: payload}); err != nil {
				return "", err
			}
		}
		if err := e.store.Complete(e.cp, StageRepair, StageRecord{InputDigest: e.inputDigest, ParamsDigest: e.params,
			OutputDigest: sha256Hex(payload), RepairDigest: rmap.Digest, SegmentsDone: len(e.plan.Segments)}); err != nil {
			return "", err
		}
		if e.opt.InterruptAfter == StageRepair {
			return StageRepair, nil
		}
	} else {
		cacheHit(e, StageRepair)
	}

	prep, err := probe.Validate(e.build.representations, e.opt.Policy)
	if err != nil {
		return "", err
	}
	sum.AVSyncMs = avSync(prep, e.build)
	sum.P95LatencyMs = p95Latency(e.build.set.Latencies)
	sum.Warnings = append(sum.Warnings, findingDetails(prep)...)

	if !done(StageProbe) {
		payload, err := json.Marshal(probeCache{Sync: sum.AVSyncMs, P95: sum.P95LatencyMs, Findings: len(prep.Findings)})
		if err != nil {
			return "", err
		}
		if !cacheHit(e, StageProbe) {
			if err := e.store.Begin(e.cp, StageProbe, e.inputDigest, e.params); err != nil {
				return "", err
			}
			if _, err := e.cache.Store(Artifact{Stage: StageProbe, SourceID: artSource, InputDigest: e.inputDigest,
				ParamsDigest: e.params, Payload: payload}); err != nil {
				return "", err
			}
		}
		if err := e.store.Complete(e.cp, StageProbe, StageRecord{InputDigest: e.inputDigest, ParamsDigest: e.params,
			OutputDigest: sha256Hex(payload)}); err != nil {
			return "", err
		}
		if e.opt.InterruptAfter == StageProbe {
			return StageProbe, nil
		}
	} else {
		cacheHit(e, StageProbe)
	}

	// Stage: index. Always rebuilt when the stage is pending because the output
	// digest depends on the artefact, and a cache hit is confirmed by Load.
	idxOut, err := buildIndex(e.build, e.plan, filepath.Join(e.opt.DataDir, "scratch"))
	if err != nil {
		return "", err
	}
	sum.IndexPoints = idxOut.points
	sum.IndexValid = idxOut.valid
	sum.OutputSHA256 = idxOut.outputDigest
	if !done(StageIndex) {
		if err := e.store.Begin(e.cp, StageIndex, e.inputDigest, e.params); err != nil {
			return "", err
		}
		payload, err := json.Marshal(indexCache{Points: idxOut.points, Valid: idxOut.valid, Output: idxOut.outputDigest})
		if err != nil {
			return "", err
		}
		if _, err := e.cache.Store(Artifact{Stage: StageIndex, SourceID: artSource, InputDigest: e.inputDigest,
			ParamsDigest: e.params, Payload: payload}); err != nil {
			return "", err
		}
		if err := e.store.Complete(e.cp, StageIndex, StageRecord{InputDigest: e.inputDigest, ParamsDigest: e.params,
			OutputDigest: sha256Hex(payload)}); err != nil {
			return "", err
		}
		if e.opt.InterruptAfter == StageIndex {
			return StageIndex, nil
		}
	} else if _, err := e.cache.Load(StageIndex, artSource, e.inputDigest, e.params, &e.sum.CacheHits); err == nil {
		// Counted by Load.
	}
	return "", nil
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
