package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/pairwise/relay/internal/cache"
	"github.com/pairwise/relay/internal/checkpoint"
	"github.com/pairwise/relay/internal/media"
)

const checkpointName = "checkpoint.json"

// Run executes an incremental relay pipeline. A fresh run with
// InterruptAfter set returns Interrupted=true after atomically checkpointing
// the requested common segment; callers then invoke Run with Resume=true.
func Run(ctx context.Context, in Input, workDir string) (*Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateInput(&in); err != nil {
		return nil, err
	}
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return nil, media.NewExitError(2, err)
	}
	if err := os.MkdirAll(absWork, 0o750); err != nil {
		return nil, media.NewExitError(4, err)
	}
	inputSHA, err := Fingerprint(in)
	if err != nil {
		return nil, err
	}
	cas, err := cache.Open(filepath.Join(absWork, "cache"))
	if err != nil {
		return nil, err
	}
	cpPath := filepath.Join(absWork, checkpointName)

	var cp *checkpoint.Checkpoint
	if in.Resume {
		cp, err = checkpoint.Load(cpPath)
		if err != nil {
			return nil, err
		}
		if err := cp.ValidateInput(inputSHA); err != nil {
			return nil, err
		}
		if cp.Completed {
			if _, err := cp.VerifyArtifacts(absWork); err != nil {
				return nil, err
			}
		}
	} else {
		cp = &checkpoint.Checkpoint{
			Seed: in.Seed, Rungs: in.Rungs, Segments: in.Segments,
			InputSHA256: inputSHA,
			Stages: []checkpoint.StageState{
				{Name: "generate", Completed: true, Digest: inputSHA},
			},
		}
		if err := checkpoint.Save(cpPath, cp); err != nil {
			return nil, err
		}
		for _, forged := range in.ForgeCache {
			if _, err := cas.Put(forged.Key, forged.Data); err != nil {
				return nil, err
			}
		}
	}

	trs, transportStats, err := resolveTransport(ctx, in)
	if err != nil {
		return nil, err
	}
	cp.SetStage("transport", checkpoint.HashBytes([]byte(transportSummary(transportStats))), true)

	store, interrupted, err := processSegments(ctx, in, cas, cp, cpPath, trs)
	if err != nil {
		return nil, err
	}
	cacheStats := cas.Stats()
	if interrupted {
		cp.InterruptedAfter = in.InterruptAfter
		cp.SetStage("schedule", "interrupted-at-"+segmentStageName(uint64(in.InterruptAfter)), false)
		if err := checkpoint.Save(cpPath, cp); err != nil {
			return nil, err
		}
		return &Result{
			WorkDir: absWork, InputSHA256: inputSHA, Segments: in.Segments, Renditions: in.Rungs,
			CacheStats: cacheStats, TransportStats: transportStats, Checkpoint: cp, Interrupted: true,
		}, nil
	}

	out, err := buildAndWriteOutputs(in, absWork, trs, store)
	if err != nil {
		return nil, err
	}
	damageChecked, damageDelta, damageFaults := verifyDamage(in)

	cp.ProcessedSegments = in.Segments
	cp.Artifacts = out.artifacts
	cp.OutputSHA256 = out.sha
	cp.Completed = true
	cp.SetStage("timeline", checkpoint.HashBytes([]byte("aligned="+itoa(out.report.Aligned))), true)
	cp.SetStage("manifest", out.sha, true)
	cp.SetStage("cache", checkpoint.HashBytes([]byte(cacheStatsSummary(cas.Stats()))), true)
	if err := checkpoint.Save(cpPath, cp); err != nil {
		return nil, err
	}
	if _, err := cp.VerifyArtifacts(absWork); err != nil {
		return nil, err
	}

	gaps := uniqueUint64(out.report.GapSegments())
	return &Result{
		WorkDir: absWork, InputSHA256: inputSHA, ArtifactSHA256: out.sha, Artifacts: out.artifacts,
		Aligned: out.report.Aligned, Segments: in.Segments, Renditions: in.Rungs,
		DamageChecked: damageChecked, DamageOffsetDelta: damageDelta,
		CacheStats: cas.Stats(), TransportStats: transportStats,
		GapSegments: gaps, Discontinuities: out.report.Discontinuities,
		Faults: damageFaults, Checkpoint: cp,
	}, nil
}

// Verify reloads a checkpoint, recomputes output hashes and rehashes every
// cache object referenced by a written media artifact.
func Verify(ctx context.Context, in Input, workDir string) (*Result, error) {
	if err := validateInput(&in); err != nil {
		return nil, err
	}
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return nil, media.NewExitError(2, err)
	}
	inputSHA, err := Fingerprint(in)
	if err != nil {
		return nil, err
	}
	cp, err := checkpoint.Load(filepath.Join(absWork, checkpointName))
	if err != nil {
		return nil, err
	}
	if err := cp.ValidateInput(inputSHA); err != nil {
		return nil, err
	}
	if !cp.Completed || cp.OutputSHA256 == "" {
		return nil, media.NewExitError(4, media.NewFault(media.CodeCheckpoint, media.FragmentID{}, -1, true, "checkpoint is not complete"))
	}
	arts, err := cp.VerifyArtifacts(absWork)
	if err != nil {
		return nil, err
	}
	cas, err := cache.Open(filepath.Join(absWork, "cache"))
	if err != nil {
		return nil, err
	}
	artifactPaths := make(map[string]bool, len(arts))
	for _, a := range arts {
		artifactPaths[a.Path] = true
	}
	for _, rr := range in.RungInputs {
		for seg := uint64(1); seg <= uint64(in.Segments); seg++ {
			rel := filepath.ToSlash(filepath.Join("media", rr.ID, segmentFileName(seg)))
			if !artifactPaths[rel] {
				continue // explicit GAP section has no media artifact/cache entry
			}
			key := cache.Key{SourceNode: rr.SourceNode, StreamID: in.StreamID, Rendition: rr.ID, Segment: seg}
			if _, _, err := cas.Get(key); err != nil {
				return nil, media.NewExitError(4, err)
			}
		}
	}
	return &Result{
		WorkDir: absWork, InputSHA256: inputSHA, ArtifactSHA256: cp.OutputSHA256, Artifacts: arts,
		Segments: cp.Segments, Renditions: cp.Rungs, CacheStats: cas.Stats(), Checkpoint: cp,
	}, nil
}

// Fingerprint hashes all deterministic input data while ignoring run control
// flags such as resume and interrupt position.
func Fingerprint(in Input) (string, error) {
	in.Resume = false
	in.InterruptAfter = 0
	raw, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	return checkpoint.HashBytes(raw), nil
}

func validateInput(in *Input) error {
	if in.Segments <= 0 || in.Segments > 10000 {
		return media.NewExitError(2, media.NewFault(media.CodeInvalidArgument, media.FragmentID{}, -1, false, "segments must be in 1..10000"))
	}
	if in.Rungs <= 0 || in.Rungs > 8 {
		return media.NewExitError(2, media.NewFault(media.CodeInvalidArgument, media.FragmentID{}, -1, false, "rungs must be in 1..8"))
	}
	if in.Duration90k == 0 {
		in.Duration90k = Duration90k
	}
	if in.Duration90k <= 1 || in.Duration90k > 90_000*60 {
		return media.NewExitError(2, media.NewFault(media.CodeInvalidArgument, media.FragmentID{}, -1, false, "duration90k is out of range"))
	}
	if in.Window == 0 {
		in.Window = 8
	}
	if in.Window > 64 {
		return media.NewExitError(2, media.NewFault(media.CodeInvalidArgument, media.FragmentID{}, -1, false, "reorder window exceeds 64"))
	}
	if in.StreamID == "" {
		return media.NewExitError(2, media.NewFault(media.CodeInvalidArgument, media.FragmentID{}, -1, false, "stream id is required"))
	}
	if len(in.RungInputs) != in.Rungs {
		return media.NewExitError(2, media.NewFault(media.CodeInvalidArgument, media.FragmentID{}, -1, false, "rung input count %d != rungs %d", len(in.RungInputs), in.Rungs))
	}
	for _, r := range in.RungInputs {
		if err := cache.ValidateKey(cache.Key{SourceNode: r.SourceNode, StreamID: in.StreamID, Rendition: r.ID, Segment: 1}); err != nil {
			return media.NewExitError(2, err)
		}
		if r.Timescale == 0 {
			return media.NewExitError(2, media.NewFault(media.CodeInvalidArgument, media.FragmentID{Rendition: r.ID}, -1, false, "timescale required"))
		}
	}
	return nil
}

func segmentFileName(seg uint64) string {
	return "seg-" + fiveDigits(seg) + ".m4s"
}
