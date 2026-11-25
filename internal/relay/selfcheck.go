package relay

import (
	"context"
	"fmt"
	"os"

	"relay.local/relay/internal/audit"
	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/scene"
)

// verifyDeterminism re-renders the composition into a scratch directory and
// compares per-segment digests with the primary run. Any difference means the
// pipeline is not reproducible and the run must exit 4.
func verifyDeterminism(ctx context.Context, sc *scene.Scenario, opts Options, rep *Report, first []string) error {
	segA := audit.SplitSegments(first, sc.Params.Segments)
	if len(segA) == 0 {
		return fault.New(-1, 0, fault.KindCorrupt, false, "no rendered frames to verify")
	}
	tmp, err := os.MkdirTemp("", "relay-verify-*")
	if err != nil {
		return fault.Wrap(-1, 0, fault.KindCorrupt, false, err)
	}
	defer os.RemoveAll(tmp)
	second, err := renderAll(ctx, sc, opts, tmp, 0, nil)
	if err != nil {
		return err
	}
	segB := audit.SplitSegments(second, sc.Params.Segments)
	matches, total := 0, 0
	for i := range segA {
		total++
		if i < len(segB) && segA[i].Hash == segB[i].Hash {
			matches++
		}
	}
	rep.SHA256Match = fmt.Sprintf("%d/%d", matches, total)
	if matches != total || len(segB) != len(segA) {
		rep.Exit = ExitHashDrift
		rep.Notes = append(rep.Notes, "segment digests differ between runs")
	}
	return nil
}

// selfcheckExtra verifies the cancellation and resume path: a cancelled run
// must leave a resumable checkpoint whose completion reproduces the
// uninterrupted segment digests, and transport recovery must match the
// generator's ground truth.
func selfcheckExtra(ctx context.Context, sc *scene.Scenario, opts Options, rep *Report,
	first []string, recs []streamResult) error {
	segA := audit.SplitSegments(first, sc.Params.Segments)

	cancelDir, err := os.MkdirTemp("", "relay-cancel-*")
	if err != nil {
		return fault.Wrap(-1, 0, fault.KindCorrupt, false, err)
	}
	defer os.RemoveAll(cancelDir)
	cancelAt := len(first) / 3
	if cancelAt < 1 {
		cancelAt = 1
	}
	_, cerr := renderAll(ctx, sc, opts, cancelDir, cancelAt, nil)
	if !isCancel(cerr) {
		return fault.New(-1, 0, fault.KindCorrupt, false,
			"cancellation path did not report a cancelled fault: %v", cerr)
	}
	// Resume from the checkpoint the cancelled run wrote. The deterministic
	// pipeline must reproduce every frame, so the resumed directory has to
	// equal the uninterrupted one segment for segment.
	resumeOpts := opts
	resumeOpts.Resume = true
	resumed, rerr := renderAll(ctx, sc, resumeOpts, cancelDir, 0, nil)
	if rerr != nil {
		return rerr
	}
	segC := audit.SplitSegments(resumed, sc.Params.Segments)
	equal := len(segC) == len(segA)
	for i := range segC {
		if i >= len(segA) || segC[i].Hash != segA[i].Hash {
			equal = false
			break
		}
	}
	if !equal {
		rep.Exit = ExitHashDrift
		rep.Notes = append(rep.Notes, "resumed run does not reproduce the uninterrupted segment digests")
		return nil
	}
	rep.Cancelled = 1

	// Transport verification: every stream must have been recovered exactly.
	okStreams := 0
	for _, r := range recs {
		if r.ok() {
			okStreams++
		}
	}
	rep.Recovered = fmt.Sprintf("%d/%d", okStreams, len(recs))
	if okStreams != len(recs) {
		rep.Exit = ExitHashDrift
		rep.Notes = append(rep.Notes, "transport recovery did not match the generator ground truth")
	}
	return nil
}
