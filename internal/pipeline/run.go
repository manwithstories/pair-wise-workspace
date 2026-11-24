package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediad/internal/core"
	"mediad/internal/manifest"
	"mediad/internal/source"
)

// ExecuteParams configures one execution.
type ExecuteParams struct {
	MediaRoot          string
	ManifestPath       string
	CheckpointPath     string
	Case               string
	PathPrefix         string
	VerifyBaseDir      string
	ExpectedHash       string
	Seed               int64
	Config             Config
	CancelAfterCommits int
	Clock              core.Clock
	Existing           *Checkpoint
}

func quarantineFrom(rel, prefix string, oc source.Outcome) manifest.QuarantineRecord {
	kind, msg := "", ""
	var offset int64 = -1
	if oc.Fault != nil {
		kind = string(oc.Fault.Kind)
		offset = oc.Fault.Offset
		msg = oc.Fault.Error()
	}
	return manifest.QuarantineRecord{
		Path: prefixed(prefix, rel), SHA256: oc.Src.SHA256, Size: oc.Src.Size,
		Kind: kind, Offset: offset, Error: msg,
	}
}

// Execute discovers inputs, runs the bounded pool, commits complete records in
// deterministic order, and (unless canceled) writes and verifies the manifest.
func Execute(ctx context.Context, p ExecuteParams) (*Report, error) {
	cfg := p.Config.Normalized()
	clock := p.Clock
	if clock == nil {
		clock = core.SystemClock{}
	}

	plan, err := source.Discover(p.MediaRoot)
	if err != nil {
		return nil, err
	}
	meta := loadMetadata(plan)

	cp := p.Existing
	resumed := 0
	if cp == nil {
		cp = NewCheckpoint(p.CheckpointPath, p.Case, p.Seed, plan.InputDigest, cfg.Digest(), p.ExpectedHash, clock)
		cp.Processing = cfg
	} else {
		resumed = countPending(plan, cp)
	}

	pending := make([]source.Job, 0, len(plan.Fragments))
	for _, s := range plan.Fragments {
		if cp.Has(s.Rel) {
			continue
		}
		ts := uint32(90000)
		if m, ok := meta[s.Representation]; ok && m.timescale != 0 {
			ts = m.timescale
		}
		sidecar := ""
		if rs, ok := plan.RTP[s.Rel]; ok {
			sidecar = rs.Abs
		}
		pending = append(pending, source.Job{
			Index: len(pending), Src: s, Timescale: ts, RTPClock: 90000, RTPSidecar: sidecar,
		})
	}

	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobsCh := make(chan source.Job, cfg.Queue)
	resultsCh := make(chan source.Outcome, cfg.Queue)
	wait := source.SpawnWorkers(childCtx, cfg.Workers, jobsCh, resultsCh)
	go func() {
		defer close(jobsCh)
		for _, j := range pending {
			select {
			case jobsCh <- j:
			case <-childCtx.Done():
				return
			}
		}
	}()

	buf := map[int]source.Outcome{}
	next, done, newCommits, duplicateEmit := 0, 0, 0, 0
	tripped := false

commitLoop:
	for done < len(pending) {
		select {
		case oc := <-resultsCh:
			buf[oc.Index] = oc
			for {
				oc, ok := buf[next]
				if !ok {
					break
				}
				if err := childCtx.Err(); err != nil {
					tripped = true
					break commitLoop
				}
				delete(buf, next)
				rel := oc.Src.Rel
				if cp.Has(rel) {
					duplicateEmit++
				} else if oc.Fault != nil {
					cp.AddQuarantine(rel, quarantineFrom(rel, p.PathPrefix, oc))
					if err := cp.Save(clock); err != nil {
						cancel()
						wait()
						return nil, err
					}
				} else {
					cp.AddFragment(CommittedItem{Rel: rel, Obs: oc.Observation, RoundTripOK: oc.RoundTripOK})
					if err := cp.Save(clock); err != nil {
						cancel()
						wait()
						return nil, err
					}
				}
				done++
				next++
				newCommits++
				if p.CancelAfterCommits > 0 && newCommits >= p.CancelAfterCommits {
					tripped = true
					cp.State = StateCanceled
					_ = cp.Save(clock)
					cancel()
					break commitLoop
				}
			}
		case <-childCtx.Done():
			tripped = true
			break commitLoop
		}
	}
	cancel()
	wait()

	rep := &Report{
		Committed: len(cp.Committed), Resumed: resumed, DuplicateEmit: duplicateEmit,
		InputDigest: plan.InputDigest, ConfigDigest: cfg.Digest(),
		ExpectedHash: cp.ExpectedResultHash,
	}

	if tripped {
		cp.State = StateCanceled
		if err := cp.Save(clock); err != nil {
			return nil, err
		}
		rep.Canceled = true
		rep.ExitCode = core.ExitCanceled
		return rep, nil
	}

	mf, hash, _ := buildManifest(cp, plan, meta, p.Case, p.Seed, p.PathPrefix, cfg)
	mf.Stats.Resumed = resumed
	mf.Stats.DuplicateEmit = duplicateEmit
	mf.ResultHash = hash
	if p.ManifestPath != "" {
		if err := manifest.WriteJSONAtomic(p.ManifestPath, mf); err != nil {
			return nil, err
		}
	}
	base := p.VerifyBaseDir
	if base == "" {
		base = p.MediaRoot
	}
	vr := manifest.VerifyManifest(base, mf)
	rep.Manifest, rep.Verify, rep.ResultHash = mf, vr, hash
	rep.ResultHashEqual = cp.ExpectedResultHash == "" || cp.ExpectedResultHash == hash

	cp.State = StateComplete
	cp.ResultHash = hash
	if err := cp.Save(clock); err != nil {
		return nil, err
	}
	if !vr.Complete() {
		return rep, &core.InputError{Err: fmt.Errorf("artifact verification failed: missing=%v sha=%v roundtrip=%v",
			vr.Missing, vr.SHAFailed, vr.RoundTripFailed)}
	}
	if len(cp.Quarantine) > 0 {
		rep.ExitCode = core.ExitQuarantined
	} else {
		rep.ExitCode = core.ExitComplete
	}
	return rep, nil
}

func countPending(plan *source.Plan, cp *Checkpoint) int {
	n := 0
	for _, s := range plan.Fragments {
		if !cp.Has(s.Rel) {
			n++
		}
	}
	return n
}

// DryRun computes the deterministic result hash without writing a checkpoint
// or manifest. The cancel demo uses it as the pinned expected hash.
func DryRun(ctx context.Context, mediaRoot string, cfg Config) (*Report, error) {
	return Execute(ctx, ExecuteParams{MediaRoot: mediaRoot, Config: cfg.Normalized()})
}

// ResumeDir loads <dir>/checkpoint.json, verifies digest bindings and reruns
// only uncommitted fragments. A completed checkpoint is idempotent.
func ResumeDir(ctx context.Context, dir string) (*Report, error) {
	cpPath := filepath.Join(dir, CheckpointName)
	cp, err := LoadCheckpoint(cpPath)
	if err != nil {
		return nil, err
	}
	cfg := cp.Processing.Normalized()
	if cfg.Digest() != cp.ConfigDigest {
		return nil, &core.ConfigError{Err: fmt.Errorf("config digest mismatch: checkpoint=%s current=%s", cp.ConfigDigest, cfg.Digest())}
	}
	mediaRoot := filepath.Join(dir, cp.MediaDir)
	plan, err := source.Discover(mediaRoot)
	if err != nil {
		return nil, err
	}
	if plan.InputDigest != cp.InputDigest {
		return nil, &core.InputError{Err: fmt.Errorf("input digest mismatch: checkpoint=%s disk=%s (inputs changed)", cp.InputDigest, plan.InputDigest)}
	}

	manifestPath := filepath.Join(dir, "manifest.json")
	if cp.State == StateComplete {
		rep := &Report{
			ExitCode: core.ExitComplete, Committed: len(cp.Committed), Resumed: 0, DuplicateEmit: 0,
			ResultHash: cp.ResultHash, ExpectedHash: cp.ExpectedResultHash,
			ResultHashEqual: cp.ExpectedResultHash == "" || cp.ExpectedResultHash == cp.ResultHash,
		}
		if b, rerr := os.ReadFile(manifestPath); rerr == nil {
			var mf manifest.Manifest
			if json.Unmarshal(b, &mf) == nil {
				rep.Manifest = &mf
				rep.Verify = manifest.VerifyManifest(dir, &mf)
				if !rep.Verify.Complete() {
					return rep, &core.InputError{Err: fmt.Errorf("existing manifest verification failed")}
				}
				if len(mf.Quarantined) > 0 {
					rep.ExitCode = core.ExitQuarantined
				}
			}
		}
		return rep, nil
	}

	return Execute(ctx, ExecuteParams{
		Existing: cp, CheckpointPath: cpPath, MediaRoot: mediaRoot,
		ManifestPath: manifestPath, PathPrefix: cp.MediaDir, VerifyBaseDir: dir,
		Config: cfg, Clock: core.SystemClock{}, ExpectedHash: cp.ExpectedResultHash,
	})
}
