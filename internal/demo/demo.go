// Package demo orchestrates the end-to-end acceptance flow: generate fixtures,
// scan them with cache measurement, stream them through audio conversion and
// RTP recovery, aggregate metrics and render the JSON report.
package demo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"mediaroute/internal/clock"
	merr "mediaroute/internal/errors"
	"mediaroute/internal/fixtures"
	"mediaroute/internal/index"
	"mediaroute/internal/manifest"
	"mediaroute/internal/metrics"
	"mediaroute/internal/scan"
	"mediaroute/internal/stream"
)

// Options configures a demo run.
type Options struct {
	WorkDir       string
	Seed          int64
	Repeat        int
	LossBurst     int
	TruncateTail  int
	ReorderWindow int
	KeepWorkDir   bool
	// ReuseExisting keeps any media already present in WorkDir instead of
	// regenerating it, so an operator can mutate a fixture and observe that the
	// cached index is rejected and rebuilt.
	ReuseExisting bool
	// DurationScale multiplies fixture length; used to prove memory stays
	// bounded as duration grows.
	DurationScale int
	// LayoutOverride replaces the declared audio layout, including "unknown"
	// or "" to exercise the explicit fallback policy.
	LayoutOverride string
	Quiet          bool
	JSON           bool
	Progress       io.Writer
}

// Take is one run's report plus its stable artifact digest.
type Take struct {
	Report   metrics.Report
	Artifact string
	ExitCode int
}

// Result is the whole demo outcome. Only the normal asset is published in the
// headline report; every fault variant is verified for exit codes and index
// boundaries separately.
type Result struct {
	Takes    []Take
	Stable   bool
	ExitCode int
	WorkDir  string
	Faults   []FaultCheck
}

// FaultCheck validates one corruption variant's expected behaviour.
type FaultCheck struct {
	Variant          fixtures.Variant
	Recoverable      bool
	CorruptRanges    int
	IndexedSamples   int
	IsolatedSamples  int
	StreamBoundaryOK bool
	Detail           string
}

// Run executes the demo.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.Progress == nil {
		opts.Progress = io.Discard
	}
	if err := validate(opts); err != nil {
		return nil, err
	}
	dir := opts.WorkDir
	cleanup := func() {}
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "mediaroute-demo-*")
		if err != nil {
			return nil, merr.New(merr.KindConfig, 0, -1, -1, false, "create work dir: %v", err)
		}
		if !opts.KeepWorkDir {
			cleanup = func() { os.RemoveAll(dir) }
		}
	}
	defer cleanup()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, merr.New(merr.KindConfig, 0, -1, -1, false, "create work dir: %v", err)
	}

	spec := fixtures.DefaultSpec(opts.Seed)
	if opts.LayoutOverride != "" {
		spec.Layout = opts.LayoutOverride
	}
	if opts.TruncateTail > 0 {
		spec.TruncateTailBytes = opts.TruncateTail
	}
	if opts.DurationScale > 1 {
		spec.VideoSamples *= opts.DurationScale
		spec.AudioSamples *= opts.DurationScale
	}
	store := &index.Store{Dir: filepath.Join(dir, "index-cache")}
	result := &Result{WorkDir: dir}
	repeat := opts.Repeat
	if repeat <= 0 {
		repeat = 1
	}
	for i := 0; i < repeat; i++ {
		if err := ctx.Err(); err != nil {
			return finish(result, dir), merr.New(merr.KindCanceled, 0, -1, -1, false, "demo canceled: %v", err)
		}
		// The first run builds media; later runs reuse it byte-for-byte so the
		// cache can prove a hit and the artifact digest can prove stability.
		reuse := i > 0 || opts.ReuseExisting
		take, err := runOnce(ctx, dir, spec, store, opts, reuse)
		if err != nil {
			return finish(result, dir), err
		}
		result.Takes = append(result.Takes, *take)
	}
	result.Stable = true
	if len(result.Takes) > 1 {
		first := result.Takes[0].Artifact
		for _, t := range result.Takes[1:] {
			if t.Artifact != first {
				result.Stable = false
			}
		}
	}
	if !opts.JSON {
		fmt.Fprintf(opts.Progress, "checking fault variants…\n")
	}
	faults, err := checkFaults(ctx, dir, spec, opts)
	if err != nil {
		return finish(result, dir), err
	}
	result.Faults = faults
	result.ExitCode = result.Takes[len(result.Takes)-1].ExitCode
	if !result.Stable {
		result.ExitCode = 3
	}
	return finish(result, dir), nil
}

func finish(result *Result, dir string) *Result {
	result.WorkDir = dir
	return result
}

func validate(opts Options) error {
	switch {
	case opts.Seed < 0:
		return merr.New(merr.KindConfig, 0, -1, -1, false, "seed must be non-negative")
	case opts.Repeat < 0:
		return merr.New(merr.KindConfig, 0, -1, -1, false, "repeat must be non-negative")
	case opts.TruncateTail < 0:
		return merr.New(merr.KindConfig, 0, -1, -1, false, "truncate-tail must be non-negative")
	case opts.TruncateTail > 0 && opts.TruncateTail < 1024:
		return merr.New(merr.KindConfig, 0, -1, -1, false, "truncate-tail must be 0 or at least 1024 bytes")
	case opts.LossBurst < 0:
		return merr.New(merr.KindConfig, 0, -1, -1, false, "loss-burst must be non-negative")
	case opts.LossBurst > 200:
		return merr.New(merr.KindConfig, 0, -1, -1, false, "loss-burst must be at most 200")
	case opts.ReorderWindow <= 0:
		return merr.New(merr.KindConfig, 0, -1, -1, false, "reorder-window must be positive")
	case opts.ReorderWindow > 4096:
		return merr.New(merr.KindConfig, 0, -1, -1, false, "reorder-window must be at most 4096")
	}
	return nil
}

// runOnce builds the healthy asset, scans it and streams one report.
func runOnce(ctx context.Context, dir string, spec fixtures.Spec, store *index.Store, opts Options, reuse bool) (*Take, error) {
	spec.KeepExisting = reuse
	asset, err := fixtures.Build(dir, spec, fixtures.VariantNormal)
	if err != nil {
		return nil, merr.New(merr.KindInternal, 0, -1, -1, false, "build fixture: %v", err)
	}
	m, err := manifest.ReadFile(asset.Manifest)
	if err != nil {
		return nil, err
	}
	if !opts.Quiet {
		fmt.Fprintf(opts.Progress, "scan: %s\n", m)
	}
	scanRes, err := scan.Run(ctx, m, scan.Options{BaseDir: dir, Cache: store, VerifyBytes: true})
	if err != nil {
		return nil, err
	}
	return process(ctx, dir, scanRes, opts)
}

// process streams a scan result and folds it into a report.
func process(ctx context.Context, dir string, scanRes *scan.Result, opts Options) (*Take, error) {
	t0 := time.Now()
	coll := metrics.New()
	sinkCtx := stream.WithSink(ctx, coll.Event)
	streamRes, streamErr := stream.Run(sinkCtx, scanRes.Index, stream.Options{
		BaseDir:          dir,
		GenerateRTP:      true,
		ReorderWindow:    opts.ReorderWindow,
		LossBurst:        opts.LossBurst,
		LoudnessWindowMS: 400,
	})
	if streamErr != nil {
		if e, ok := merr.As(streamErr); !ok || !e.Recoverable {
			return nil, streamErr
		}
	}
	observeSync(coll, scanRes.Index)
	rep := coll.Finalize(streamRes, indexView{scanRes.Index}, scanRes.Rebuilt, scanRes.CacheHit, spans(scanRes.Index))
	rep.ElapsedMS = float64(time.Since(t0).Microseconds()) / 1000
	digest, err := artifactDigest(rep)
	if err != nil {
		return nil, err
	}
	rep.ArtifactSummary = digest
	if opts.LossBurst > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("synthetic loss burst of %d packets injected", opts.LossBurst))
	}
	take := &Take{Report: rep, Artifact: digest}
	take.ExitCode = exitFor(rep)
	return take, nil
}

// exitFor maps a healthy-asset report to the documented exit codes.
func exitFor(rep metrics.Report) int {
	if rep.CorruptRanges > 0 || rep.IsolatedSamples > 0 {
		return 2
	}
	if err := rep.Check(); err != nil {
		// Metric thresholds on the normal asset are an acceptance failure.
		return 2
	}
	return 0
}

// observeSync derives cross-track deviations from scanned sample times. It
// keeps source, edit-mapped and output times so every deviation can be
// verified independently.
func observeSync(coll *metrics.Collector, ix *index.Index) {
	if ix == nil {
		return
	}
	type ref struct {
		ms  float64
		dev float64
	}
	var video, audio []float64
	var videoSamples []index.Sample
	var videoTrack index.Track
	for _, tr := range ix.Tracks {
		switch tr.Kind {
		case "video":
			videoTrack = tr
			for _, s := range tr.Samples {
				if s.Readable {
					video = append(video, sampleMS(tr, s.DTS))
					videoSamples = append(videoSamples, s)
				}
			}
		case "audio":
			for _, s := range tr.Samples {
				if s.Readable {
					audio = append(audio, sampleMS(tr, s.DTS))
				}
			}
		}
	}
	if len(video) == 0 || len(audio) == 0 {
		return
	}
	sort.Float64s(audio)
	for i, ms := range video {
		dev := metrics.SyncDeviation(ms, audio)
		s := videoSamples[i]
		coll.ObserveSyncDetail(metrics.SyncSample{
			TrackID:    videoTrack.ID,
			Sample:     s.Number,
			Source:     rationalMS(s.DTS, videoTrack),
			EditMapped: rationalMS(s.DTS-videoTrack.EditMediaTicks, videoTrack),
			Output:     rationalMS(s.CTS-videoTrack.EditMediaTicks, videoTrack),
			Deviation:  dev,
		})
	}
}

// sampleTimeline converts a media tick position to absolute timeline seconds
// using the same edit-media rule as the streaming stage.
func sampleTimeline(tr index.Track, ticks int64) clock.Rational {
	if ticks < tr.EditMediaTicks {
		return tr.Origin
	}
	r, err := clock.FromTicks(ticks-tr.EditMediaTicks, tr.Timescale)
	if err != nil {
		return clock.Rational{Den: 1}
	}
	if sum, err := r.Add(tr.Origin); err == nil {
		return sum
	}
	return r
}

func sampleMS(tr index.Track, dts int64) float64 {
	return sampleTimeline(tr, dts).Milliseconds()
}

func rationalMS(ticks int64, tr index.Track) clock.Rational {
	return sampleTimeline(tr, ticks)
}

func spans(ix *index.Index) []metrics.CorruptSpan {
	if ix == nil {
		return nil
	}
	out := make([]metrics.CorruptSpan, 0, len(ix.CorruptRanges))
	for _, r := range ix.CorruptRanges {
		out = append(out, metrics.CorruptSpan{
			TrackID: r.TrackID, FirstSample: r.FirstSample, LastSample: r.LastSample,
			ByteOffset: r.ByteOffset, Reason: r.Reason,
			StartMS: r.Start.Milliseconds(), EndMS: r.End.Milliseconds(),
		})
	}
	return out
}

// artifactDigest hashes the report fields that must be identical across
// reruns. Timing metrics, the digest itself and per-sample audit rows are
// excluded because they legitimately vary with machine load.
func artifactDigest(rep metrics.Report) (string, error) {
	cp := rep
	cp.ElapsedMS = 0
	cp.LatencyMSP95 = 0
	cp.LatencyMSMax = 0
	cp.ArtifactSummary = ""
	cp.SyncSamples = nil
	cp.Warnings = nil
	// Cache telemetry describes the run, not the produced artifact: the index
	// and the media results must be identical whether or not it was cached.
	cp.CacheHit = false
	data, err := json.Marshal(&cp)
	if err != nil {
		return "", merr.New(merr.KindInternal, 0, -1, -1, false, "digest report: %v", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// indexView adapts *index.Index to the small surface metrics needs.
type indexView struct{ ix *index.Index }

func (v indexView) Samples() int {
	if v.ix == nil {
		return 0
	}
	return v.ix.IndexedSamples
}
func (v indexView) CorruptCount() int {
	if v.ix == nil {
		return 0
	}
	return len(v.ix.CorruptRanges)
}
func (v indexView) Summary() string {
	if v.ix == nil {
		return ""
	}
	return v.ix.Summary
}
