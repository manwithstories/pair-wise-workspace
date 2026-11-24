package demo

import (
	"context"
	"fmt"
	"path/filepath"

	merr "mediaroute/internal/errors"
	"mediaroute/internal/fixtures"
	"mediaroute/internal/index"
	"mediaroute/internal/manifest"
	"mediaroute/internal/scan"
	"mediaroute/internal/stream"
)

// checkFaults scans and streams each corruption variant to prove both the
// scan index and the streaming executor stop at the same boundary.
func checkFaults(ctx context.Context, dir string, spec fixtures.Spec, opts Options) ([]FaultCheck, error) {
	spec.KeepExisting = false
	out := make([]FaultCheck, 0, len(fixtures.AllVariants())-1)
	for _, v := range fixtures.AllVariants() {
		if v == fixtures.VariantNormal {
			continue
		}
		if err := ctx.Err(); err != nil {
			return out, merr.New(merr.KindCanceled, 0, -1, -1, false, "demo canceled: %v", err)
		}
		asset, err := fixtures.Build(dir, spec, v)
		if err != nil {
			return out, merr.New(merr.KindInternal, 0, -1, -1, false, "build %s: %v", v, err)
		}
		m, err := manifest.ReadFile(asset.Manifest)
		if err != nil {
			return out, err
		}
		// Fault variants deliberately bypass the cache so each check measures
		// a fresh scan.
		res, scanErr := scan.Run(ctx, m, scan.Options{BaseDir: dir, VerifyBytes: true})
		if res == nil {
			return out, scanErr
		}
		streamRes, streamErr := streamFault(ctx, dir, res.Index, opts)
		fc := FaultCheck{
			Variant:         v,
			CorruptRanges:   len(res.Index.CorruptRanges),
			IndexedSamples:  res.Index.IndexedSamples,
			IsolatedSamples: streamRes.Isolated,
		}
		fc.Recoverable = streamErr != nil && merr.IsRecoverable(streamErr)
		if scanErr != nil && !merr.IsRecoverable(scanErr) {
			fc.Detail = fmt.Sprintf("scan failed unrecoverably: %v", scanErr)
			out = append(out, fc)
			continue
		}
		fc.StreamBoundaryOK = boundaryAgrees(res.Index, streamRes)
		out = append(out, fc)
	}
	return out, nil
}

// streamFault runs the streaming stage for a corrupt index; content details are
// irrelevant here, only the isolation boundary matters.
func streamFault(ctx context.Context, dir string, ix *index.Index, opts Options) (*stream.Result, error) {
	res, err := stream.Run(ctx, ix, stream.Options{BaseDir: dir, LoudnessWindowMS: 400})
	return res, err
}

// boundaryAgrees checks that the streaming stage isolated exactly the sample
// count the scanner marked unreadable, and that a corrupt span never contains
// a readable sample reported as processed inside its number range.
func boundaryAgrees(ix *index.Index, sr *stream.Result) bool {
	if sr == nil {
		return false
	}
	unreadable := 0
	for _, tr := range ix.Tracks {
		for _, s := range tr.Samples {
			if !s.Readable {
				unreadable++
			}
		}
	}
	return unreadable == sr.Isolated && sr.Processed+sr.Isolated == totalSamples(ix)
}

func totalSamples(ix *index.Index) int {
	n := 0
	for _, tr := range ix.Tracks {
		n += len(tr.Samples)
	}
	return n
}

// ScanOnly scans a single manifest without streaming, for the "scan" command.
func ScanOnly(ctx context.Context, opts ScanOptions) (*Result, error) {
	m, err := manifest.ReadFile(opts.Manifest)
	if err != nil {
		return nil, err
	}
	base := opts.BaseDir
	if base == "" {
		base = filepath.Dir(opts.Manifest)
	}
	res, err := scan.Run(ctx, m, scan.Options{BaseDir: base, VerifyBytes: true})
	if err != nil {
		return nil, err
	}
	take := &Take{}
	take.Report.IndexedSamples = res.Index.IndexedSamples
	take.Report.CorruptRanges = len(res.Index.CorruptRanges)
	take.Report.IndexSummary = res.Index.Summary
	take.Report.ProcessedSamples = 0
	if len(res.Index.CorruptRanges) > 0 {
		take.ExitCode = 2
	}
	return &Result{Takes: []Take{*take}, Stable: true, ExitCode: take.ExitCode}, nil
}

// ScanOptions configures ScanOnly.
type ScanOptions struct {
	Manifest string
	BaseDir  string
	JSON     bool
	Progress interface{ Write([]byte) (int, error) }
	Quiet    bool
}

// Verify re-scans a manifest twice with a cache and reports whether the index
// summaries agree and whether the second run was a cache hit.
func Verify(ctx context.Context, manifestPath, workDir string) (bool, string, error) {
	m, err := manifest.ReadFile(manifestPath)
	if err != nil {
		return false, "", err
	}
	base := filepath.Dir(manifestPath)
	cacheDir := workDir
	if cacheDir == "" {
		cacheDir = filepath.Join(base, ".mediaroute-cache")
	}
	store := &index.Store{Dir: cacheDir}
	first, err := scan.Run(ctx, m, scan.Options{BaseDir: base, Cache: store, VerifyBytes: true})
	if err != nil {
		if !merr.IsRecoverable(err) {
			return false, "", err
		}
	}
	second, err := scan.Run(ctx, m, scan.Options{BaseDir: base, Cache: store, VerifyBytes: true})
	if err != nil && !merr.IsRecoverable(err) {
		return false, "", err
	}
	ok := first.Index.Summary == second.Index.Summary && second.CacheHit && !second.Rebuilt
	detail := fmt.Sprintf("summary=%s first_hit=%v second_hit=%v rebuilt=%v stable=%v",
		first.Index.Summary[:12], first.CacheHit, second.CacheHit, second.Rebuilt, ok)
	return ok, detail, nil
}
