package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"archive/internal/clock"
	"archive/internal/errs"
	"archive/internal/index"
	"archive/internal/report"
	"archive/internal/rtp"
	"archive/internal/testdata"
)

// demoConfig holds validated demo parameters.
type demoConfig struct {
	seed       int64
	burstLoss  int
	reorder    int
	bandwidth  []float64
	chunkBytes int64
	jsonOut    bool
	outDir     string
}

// acceptance thresholds from the spec
const (
	maxAVSyncP95Ms   = 20.0
	minRecoveryRate  = 0.90
	maxProcessingP95 = 5000.0
)

// runDemo executes the full demo and returns the process exit code.
func runDemo(ctx context.Context, cfg demoConfig) (int, *report.Summary) {
	workDir := cfg.outDir
	if workDir == "" {
		dir, err := os.MkdirTemp("", "archive-demo-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "cannot create work dir:", err)
			return exitVerifyFailed, nil
		}
		workDir = dir
	}
	fixtures, err := testdata.Generate(testdata.Config{
		Seed: cfg.seed, BurstLoss: cfg.burstLoss, Reorder: cfg.reorder, Bandwidth: cfg.bandwidth,
	}, filepath.Join(workDir, "fixtures"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot generate fixtures:", err)
		return exitVerifyFailed, nil
	}
	outDir := filepath.Join(workDir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "cannot create output dir:", err)
		return exitVerifyFailed, nil
	}
	sum := &report.Summary{}
	var allSync []float64
	var procTimes []float64
	// 1. good assets through the full pipeline
	for i, a := range fixtures.Good {
		rng := rand.New(rand.NewSource(cfg.seed + int64(i)*7919))
		o := processAsset(ctx, assetInput{a.Name, a.Kind, a.Dir, a.ManifestFile}, outDir, cfg.chunkBytes, rng)
		sum.Assets = append(sum.Assets, o.rep)
		procTimes = append(procTimes, o.rep.ProcessingMs)
		if o.rep.Status != "ok" {
			sum.AssetsFailed++
			fmt.Fprintln(os.Stderr, "asset failed:", o.rep.Error)
			continue
		}
		allSync = append(allSync, o.syncErrs...)
	}
	// 2. corrupt fixtures must be detected with the expected error kind
	for i, c := range fixtures.Corrupt {
		rng := rand.New(rand.NewSource(cfg.seed + int64(100+i)*7919))
		o := processAsset(ctx, assetInput{c.Name, c.Kind, c.Dir, c.ManifestFile}, outDir, cfg.chunkBytes, rng)
		sum.CorruptCasesTotal++
		if o.rep.Status == "failed" && o.errKind == c.Want {
			sum.CorruptCasesDetected++
			o.rep.Status = "detected"
		} else {
			fmt.Fprintf(os.Stderr, "corrupt case %s: want %s failure, got %q (%s)\n",
				c.Name, c.Want, o.errKind, o.rep.Error)
		}
		sum.Assets = append(sum.Assets, o.rep)
	}
	// 3. injected disk-write failure: the output path is blocked by a dir
	sum.CorruptCasesTotal++
	if detectIOFailure(ctx, cfg, fixtures, workDir) {
		sum.CorruptCasesDetected++
	}
	// 4. RTP transport recovery
	pktStats, rate, rtpErr := runRTP(cfg, fixtures.RTP)
	if rtpErr != nil {
		fmt.Fprintln(os.Stderr, "rtp stage:", rtpErr)
		return exitVerifyFailed, nil
	}
	sum.PacketRecoveryRate = rate
	sum.PacketRecovery = pktStats
	// 5. checkpoint resume: corrupt big-dash output, resume, compare
	sum.CheckpointResumeVerified = verifyCheckpointResume(ctx, cfg, fixtures, outDir)
	// 6. aggregate metrics
	sum.AVSyncErrorMs = report.Summarize(allSync)
	sum.ProcessingP95Ms = report.Percentile(procTimes, 95)
	// exit codes: 3 = asset failures, 4 = checkpoint/result validation
	if sum.AssetsFailed > 0 {
		return exitAssetFailed, sum
	}
	checks := sum.CheckpointResumeVerified &&
		sum.CorruptCasesDetected >= sum.CorruptCasesTotal &&
		sum.PacketRecoveryRate >= minRecoveryRate &&
		sum.AVSyncErrorMs.P95 <= maxAVSyncP95Ms &&
		sum.ProcessingP95Ms <= maxProcessingP95
	if !checks {
		return exitVerifyFailed, sum
	}
	return exitOK, sum
}

// detectIOFailure processes a good asset with its output path blocked by
// a directory; the pipeline must report a locatable KindIO error.
func detectIOFailure(ctx context.Context, cfg demoConfig, f *testdata.Fixtures, workDir string) bool {
	blocked := filepath.Join(workDir, "out-blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "episode-hls.mp4"), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "cannot set up io failure case:", err)
		return false
	}
	a := f.Good[0] // episode-hls
	rng := rand.New(rand.NewSource(cfg.seed + 999*7919))
	o := processAsset(ctx, assetInput{a.Name, a.Kind, a.Dir, a.ManifestFile}, blocked, cfg.chunkBytes, rng)
	if o.rep.Status == "failed" && o.errKind == string(errs.KindIO) {
		return true
	}
	fmt.Fprintf(os.Stderr, "io failure case: want io failure, got %q (%s)\n", o.errKind, o.rep.Error)
	return false
}

// runRTP feeds the transmitted stream through the recoverer, sweeping the
// bandwidth trace across the stream, and verifies the recovered
// timestamps unwrap to a monotonic sequence.
func runRTP(cfg demoConfig, packets [][]byte) (*report.PacketStats, float64, error) {
	if len(packets) == 0 {
		return nil, 0, errs.New(errs.KindPacket, "rtp", "input", "no packets", nil)
	}
	rec := rtp.NewRecoverer(cfg.bandwidth[0])
	rec.SetReorderDepth(cfg.reorder) // tolerate the declared reorder depth
	for i, raw := range packets {
		rec.SetBandwidth(cfg.bandwidth[i*len(cfg.bandwidth)/len(packets)])
		if err := rec.Feed("rtp-transport", raw); err != nil {
			return nil, 0, err
		}
	}
	rec.Flush()
	st := rec.Stats()
	// recovered timestamps must unwrap monotonically (they cross 2^32)
	var uw clock.Unwrapper
	for _, p := range rec.Out {
		if _, err := uw.Unwrap(p.Header.Timestamp); err != nil {
			return nil, 0, errs.New(errs.KindClock, "rtp-transport", "timestamp unwrap", "recovered stream not monotonic", err)
		}
	}
	ps := &report.PacketStats{
		Received: st.Received, Duplicates: st.Duplicates, Emitted: st.Emitted,
		Reordered: st.Reordered, Late: st.Late, Lost: st.Lost, Origin: st.Origin,
	}
	for _, g := range st.Gaps {
		ps.Gaps = append(ps.Gaps, report.GapRef{Start: g.Start, End: g.End, Len: g.Len()})
	}
	return ps, st.RecoveryRate(), nil
}

// verifyCheckpointResume corrupts the big-dash output mid-file, re-runs
// the pipeline (which must resume from the last valid checkpoint) and
// compares the rebuilt file with the original.
func verifyCheckpointResume(ctx context.Context, cfg demoConfig, f *testdata.Fixtures, outDir string) bool {
	idx := -1
	for i, a := range f.Good {
		if a.Name == "big-dash" {
			idx = i
		}
	}
	if idx < 0 {
		return false
	}
	a := f.Good[idx]
	outPath := filepath.Join(outDir, a.Name+".mp4")
	orig, err := os.ReadFile(outPath)
	if err != nil || len(orig) == 0 {
		fmt.Fprintln(os.Stderr, "checkpoint verify: cannot read original output:", err)
		return false
	}
	origHash := sha256.Sum256(orig)
	// damage a byte inside the last committed chunk so the resume must
	// roll back to the previous checkpoint, not to zero
	corruptAt := int64(len(orig) * 2 / 5)
	if cp, err := index.Load(outPath + ".index.json"); err == nil && cp != nil && len(cp.Chunks) > 0 {
		last := cp.Chunks[len(cp.Chunks)-1]
		start := int64(0)
		if len(cp.Chunks) > 1 {
			start = cp.Chunks[len(cp.Chunks)-2].ByteEnd
		}
		corruptAt = start + (last.ByteEnd-start)/2
	}
	corrupted := append([]byte(nil), orig...)
	corrupted[corruptAt] ^= 0xff
	if err := os.WriteFile(outPath, corrupted, 0o644); err != nil {
		return false
	}
	rng := rand.New(rand.NewSource(cfg.seed + int64(idx)*7919))
	o := processAsset(ctx, assetInput{a.Name, a.Kind, a.Dir, a.ManifestFile}, outDir, cfg.chunkBytes, rng)
	if o.rep.Status != "ok" {
		fmt.Fprintln(os.Stderr, "checkpoint verify: resume run failed:", o.rep.Error)
		return false
	}
	if o.resumePos == 0 {
		fmt.Fprintln(os.Stderr, "checkpoint verify: pipeline did not resume from a checkpoint")
		return false
	}
	final, err := os.ReadFile(outPath)
	if err != nil {
		return false
	}
	if sha256.Sum256(final) != origHash {
		fmt.Fprintln(os.Stderr, "checkpoint verify: rebuilt output differs from original")
		return false
	}
	return true
}
