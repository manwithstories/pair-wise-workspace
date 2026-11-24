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

	// Stage: index. It runs before probe because the residual audio/video
	// offset is measured from the indexed sample times.
	var syncMs int64
	if done(StageIndex) {
		if pc, ok := e.hydrateIndex(); ok {
			sum.IndexPoints = pc.Points
			sum.IndexValid = pc.Valid
			sum.OutputSHA256 = pc.Output
			syncMs = pc.SyncMs
		}
	}
	if sum.OutputSHA256 == "" {
		e.trace(StageIndex)
		idxOut, err := buildIndex(e.build, e.plan, filepath.Join(e.opt.DataDir, "scratch"))
		if err != nil {
			return "", err
		}
		sum.IndexPoints = idxOut.points
		sum.IndexValid = idxOut.valid
		sum.OutputSHA256 = idxOut.outputDigest
		syncMs = idxOut.syncMs
		payload, err := json.Marshal(indexCache{Points: idxOut.points, Valid: idxOut.valid,
			Output: idxOut.outputDigest, SyncMs: idxOut.syncMs})
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
	}

	// Stage: probe. The measured AV offset comes from the rebuilt index, and
	// the per-representation findings come from the probe policy pass.
	probePayload := func() (probeCache, []byte, error) {
		prep, err := probe.Validate(e.build.representations, e.opt.Policy)
		if err != nil {
			return probeCache{}, nil, err
		}
		pc := probeCache{Sync: syncMs, P95: p95Latency(e.build.set.Latencies), Findings: len(prep.Findings)}
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
	return "", nil
}
