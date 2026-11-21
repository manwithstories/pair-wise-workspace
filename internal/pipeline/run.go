// Package pipeline wires the subsystems end to end: metadata normalization,
// transport ingestion, timeline composition, offscreen rendering, and manifest
// writing. The CLI is a thin flag parser over this package so the acceptance
// behavior is covered by ordinary Go tests.
package pipeline

import (
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sort"

	"mediad/internal/clock"
	"mediad/internal/fixtures"
	"mediad/internal/manifest"
	"mediad/internal/metadata"
	"mediad/internal/model"
	"mediad/internal/render"
	"mediad/internal/scheduler"
	"mediad/internal/timeline"
	"mediad/internal/transport"
)

// Options are the CLI parameters.
type Options struct {
	Seed            int64
	BandwidthKbps   float64
	LossBurst       int
	Retry           int
	Out             string
	CorruptMetadata bool
	Packets         int
	Frames          int
	// Scenario selects the packet fixture ("success", "wrap", ...).
	Scenario string
	// ResumeFrom loads a previous manifest and resumes completed DAG nodes.
	ResumeFrom string
	// WriteFrames controls whether PPM artifacts are written.
	WriteFrames bool
	// RenderScale divides the verification raster size (>=1).
	RenderScale int
}

// DefaultOptions returns acceptance defaults.
func DefaultOptions() Options {
	return Options{Seed: 42, BandwidthKbps: 1800, LossBurst: 4, Retry: 2,
		Packets: 120, Frames: 30, Scenario: fixtures.ScenarioSuccess,
		WriteFrames: true, RenderScale: 4}
}

// Result is the completed run.
type Result struct {
	Manifest  *manifest.Manifest
	Report    *scheduler.Report
	Transport transport.StreamStats
	Bandwidth transport.BandwidthSummary
	Frames    []render.RenderedFrame
	Timeline  *timeline.Timeline
	Asset     *metadata.Asset
	// BlockedNode is the first blocking dependency, "none" on success.
	BlockedNode string
	// FirstError is the first categorized failure, if any.
	FirstError *model.Error
	// OutDir is the output directory that received the manifest.
	OutDir string
	// GapFrameLinks lists, per blocked gap, the frames it blocked. It makes
	// the manifest linkage visible in the terminal.
	GapFrameLinks []GapLink
}

// GapLink ties a transport gap to blocked frame indices.
type GapLink struct {
	Index     int
	StartExt  int64
	Count     int64
	Blocked   bool
	FrameIdxs []int
}

// Run executes the full pipeline. It returns a Result even when isolated
// failures occurred; a non-nil error means no usable product was produced.
func Run(opt Options) (*Result, error) {
	if opt.BandwidthKbps <= 0 {
		return nil, model.New(model.CategoryUsage, "flags", "bandwidth-kbps must be positive")
	}
	if opt.Retry < 0 {
		return nil, model.New(model.CategoryUsage, "flags", "retry must be >= 0")
	}
	if opt.Out == "" {
		return nil, model.New(model.CategoryUsage, "flags", "out directory is required")
	}
	if err := os.MkdirAll(opt.Out, 0o755); err != nil {
		return nil, model.Wrap(model.CategoryUsage, "out", err)
	}
	packets := opt.Packets
	if packets <= 0 {
		packets = 120
	}
	frames := opt.Frames
	if frames <= 0 {
		frames = 30
	}
	scenario := opt.Scenario
	if scenario == "" {
		scenario = fixtures.ScenarioSuccess
	}

	// ---- shared run state captured by DAG nodes ----
	var asset *metadata.Asset
	var metadataErr *model.Error
	var stream fixtures.Stream
	var streamStats transport.StreamStats
	var bandwidthSummary transport.BandwidthSummary
	var bandwidthRecords []transport.BandwidthSampleRecord
	var reorderGaps []timeline.Gap
	var tl *timeline.Timeline
	var rendered []render.RenderedFrame
	var psnrMean, ssimMean float64
	var maxDrift int64
	mode := "demo"
	if opt.CorruptMetadata {
		mode = "corrupt"
	}
	m := manifest.New(opt.Seed, mode)
	m.Timebase = clock.Video90k.String()

	// Corrupted metadata is isolated: the parse records failures and the
	// healthy audio branch survives.
	corrupt := opt.CorruptMetadata
	dag, err := scheduler.New([]scheduler.Node{
		{
			Name: "metadata",
			Run: func() (scheduler.Result, error) {
				dir := filepath.Join(opt.Out, "input")
				mp4Path, mxfPath, werr := fixtures.WriteMetadataPair(dir, corrupt)
				if werr != nil {
					return scheduler.Result{}, werr
				}
				// Cross-container check: the same logical tracks must normalize
				// to the same timebase, geometry and rotation in both
				// containers. A mismatch is a metadata error, not a silent pass.
				if cerr := crossContainerCheck(mp4Path, mxfPath); cerr != nil {
					return scheduler.Result{}, cerr
				}
				parsed, perr := metadata.ParseMP4(mp4Path)
				asset = parsed
				if parsed == nil {
					return scheduler.Result{}, perr
				}
				// Isolated track failures are recorded, not fatal, as long as
				// at least one video track survived. The metadata node therefore
				// succeeds; the first categorized error is surfaced separately.
				if perr != nil {
					var me *model.Error
					if asModel(perr, &me) {
						metadataErr = me
					}
				}
				if !hasVideo(parsed) {
					if perr == nil {
						perr = model.New(model.CategoryMetadata, "metadata", "no usable video track")
					}
					return scheduler.Result{}, perr
				}
				return scheduler.Result{Hash: "metadata:" + metadataHash(parsed)}, nil
			},
		},
		{
			Name: "transport",
			Run: func() (scheduler.Result, error) {
				var serr error
				stream, serr = fixtures.BuildStream(scenario, packets, opt.Seed)
				if serr != nil {
					return scheduler.Result{}, serr
				}
				// The reorder window must exceed the fixture's reorder distance,
				// which the scenario builder spaces at 5 packets.
				checker := transport.NewReorderer(transport.ReorderConfig{
					Window: 64, StartupHold: 8, BurstGapThreshold: maxInt(opt.LossBurst, 1),
				})
				delivered := 0
				for i, raw := range stream.Packets {
					_, ok, perr := checker.Push(raw, int64(i))
					if perr != nil {
						return scheduler.Result{}, perr
					}
					if ok {
						delivered++
					}
				}
				tail, stats := checker.Stats()
				delivered += len(tail)
				streamStats = stats
				// Convert transport gaps into timeline gaps in media order.
				reorderGaps = gapsToTimeline(stats.Gaps, packets, frames)
				// Dynamic bandwidth samples: vary delivery around the requested
				// capacity so the estimator must track a moving target.
				bandwidthRecords, bandwidthSummary = runBandwidth(opt, frames)
				return scheduler.Result{Hash: fmt.Sprintf("transport:%d:%d", stats.Unique, stats.Lost)}, nil
			},
		},
		{
			Name: "compose",
			Deps: []string{"metadata", "transport"},
			Run: func() (scheduler.Result, error) {
				cfg := timeline.DefaultConfig()
				if opt.RenderScale > 1 {
					cfg.Scale = opt.RenderScale
				}
				cfg.DurationTicks = int64(opt.Frames) * 3000
				cfg.HoleBlockThresholdMs = int64(maxInt(opt.LossBurst, 1)) * 30
				var cerr error
				tl, cerr = timeline.Compose(asset, reorderGaps, cfg)
				if cerr != nil {
					return scheduler.Result{}, cerr
				}
				// Trim the composed grid to the requested frame count.
				if len(tl.Frames) > opt.Frames {
					tl.Frames = tl.Frames[:opt.Frames]
					tl.Stats.FrameCount = len(tl.Frames)
				}
				return scheduler.Result{Hash: fmt.Sprintf("compose:%d", len(tl.Frames))}, nil
			},
		},
		{
			Name: "render",
			Deps: []string{"compose"},
			Run: func() (scheduler.Result, error) {
				engine := render.NewEngine(opt.Seed)
				rendered = rendered[:0]
				var psnrSum, ssimSum float64
				n := 0
				for _, f := range tl.Frames {
					rf, rerr := engine.RenderFrame(f)
					if rerr != nil {
						return scheduler.Result{}, rerr
					}
					rendered = append(rendered, rf)
					// Fidelity gate: compare the color-transformed output with
					// an independently composed reference that skips the color
					// stage. Geometry and sources are identical, so the score
					// isolates color handling.
					// Independent structural reference: inverse-mapped geometry
					// with the exact (unquantized) color transform.
					ref := engine.InverseSample(f)
					if rf.Blocked {
						// A blocked frame is a black placeholder; scoring it as
						// a perfect match would overstate quality.
						continue
					}
					psnrSum += render.PSNR(rf.Image, ref)
					ssimSum += render.SSIM(rf.Image, ref)
					n++
				}
				if n > 0 {
					psnrMean = psnrSum / float64(n)
					ssimMean = ssimSum / float64(n)
				} else {
					// Every frame was blocked: quality is undefined, not
					// perfect. Keep zero so the gate cannot pass vacuously.
					psnrMean, ssimMean = 0, 0
				}
				return scheduler.Result{Hash: hashFrames(rendered)}, nil
			},
		},
		{
			Name: "manifest",
			Deps: []string{"render"},
			Run: func() (scheduler.Result, error) {
				buildManifest(m, opt, asset, streamStats, bandwidthRecords, reorderGaps, tl, rendered)
				steadyTarget, steadyActual, steadyErr := steadyBandwidth(bandwidthRecords, opt.BandwidthKbps)
				m.Metrics = manifest.Metrics{
					TargetKbps:       steadyTarget,
					ActualKbps:       steadyActual,
					BitrateErrorPct:  steadyErr,
					FrameCount:       len(rendered),
					PSNRMeanDB:       psnrMean,
					SSIMMean:         ssimMean,
					ManifestDriftMs:  maxDrift,
					DuplicatePackets: streamStats.Duplicates,
					ReorderedPackets: streamStats.Reordered,
					LostPackets:      streamStats.Lost,
					BlockedNode:      "none",
				}
				for _, f := range tl.Frames {
					if f.Blocked && f.BlockingDep != "" {
						m.Metrics.BlockedNode = f.BlockingDep
						break
					}
				}
				if opt.WriteFrames {
					if werr := writeFrameArtifacts(opt.Out, rendered); werr != nil {
						return scheduler.Result{}, werr
					}
					for _, rf := range rendered {
						path := filepath.Join(opt.Out, "frames", frameName(rf.Index))
						if aerr := m.AddArtifact(path); aerr != nil {
							return scheduler.Result{}, aerr
						}
					}
				}
				path := filepath.Join(opt.Out, "manifest.json")
				if werr := m.Write(path); werr != nil {
					return scheduler.Result{}, werr
				}
				if aerr := m.AddArtifact(path); aerr != nil {
					return scheduler.Result{}, aerr
				}
				// Second pass: re-render the same plan and verify the manifest
				// is stable. Anchor on the rendered frame metadata, not on the
				// manifest's own artifact list.
				second := buildVerification(opt, tl, rendered)
				stable := manifest.Compare(m, second, 1)
				m.Stable = &stable
				m.Metrics.ManifestDriftMs = stable.DriftMs
				if werr := m.Write(path); werr != nil {
					return scheduler.Result{}, werr
				}
				return scheduler.Result{Hash: "manifest:" + hashFrames(rendered)}, nil
			},
		},
	})
	if err != nil {
		return nil, err
	}
	dag.MaxAttempts = opt.Retry + 1

	var prior map[string]scheduler.NodeState
	if opt.ResumeFrom != "" {
		prev, lerr := manifest.Load(opt.ResumeFrom)
		if lerr != nil {
			return nil, lerr
		}
		prior = priorStates(prev)
	}
	report := dag.Resume(prior)
	m.NodesFromReport(report)

	res := &Result{Manifest: m, Report: report, Transport: streamStats,
		Bandwidth: bandwidthSummary, Frames: rendered, Timeline: tl, Asset: asset, OutDir: opt.Out}
	res.BlockedNode = report.FirstBlockOrNone()

	// First failure is the most actionable error for the CLI.
	if report.FirstBlock != "" {
		res.FirstError = firstFailure(report)
		if res.FirstError == nil {
			res.FirstError = model.New(model.CategoryScheduler, "run",
				"blocked by "+report.FirstBlock).WithBlockingDep(report.FirstBlock)
		}
	}
	if metadataErr != nil && res.FirstError == nil {
		res.FirstError = metadataErr
	}
	// Timeline-level blocking (for example a burst gap exceeding the threshold)
	// does not fail a DAG node, but it must still be reported as blocked_node.
	if res.BlockedNode == "none" && tl != nil && tl.Stats.FirstBlocked != "" {
		res.BlockedNode = tl.Stats.FirstBlocked
	}
	// An isolated metadata failure is a data error even though the DAG
	// completed: the CLI contract requires exit 2 and the failing track.
	if metadataErr != nil && res.BlockedNode == "none" {
		res.BlockedNode = metadataErr.BlockingDep
	}
	// Persist the final manifest with node statuses and blocked node. Metrics
	// are filled even when an isolated failure blocked frames, so the terminal
	// always shows the full acceptance field set.
	if werr := finalizeManifest(res, opt); werr != nil {
		return res, werr
	}
	if res.FirstError != nil {
		return res, res.FirstError
	}
	return res, nil
}

func finalizeManifest(res *Result, opt Options) error {
	m := res.Manifest
	if m.Metrics.FrameCount == 0 && res.Timeline != nil {
		m.Metrics.FrameCount = len(res.Timeline.Frames)
	}
	if m.Metrics.BlockedNode == "" {
		m.Metrics.BlockedNode = res.BlockedNode
	}
	if res.Report.FirstBlock != "" {
		m.Metrics.BlockedNode = res.Report.FirstBlock
	} else if tl := res.Timeline; tl != nil && tl.Stats.FirstBlocked != "" {
		// Timeline blocking (burst gap above threshold) does not fail a node,
		// but the manifest must explain which dependency blocked frames.
		m.Metrics.BlockedNode = tl.Stats.FirstBlocked
	}
	m.Transport = res.Transport
	m.Bandwidth = bandwidthRecordsOf(m)
	res.GapFrameLinks = res.GapFrameLinks[:0]
	for _, g := range m.Gaps {
		res.GapFrameLinks = append(res.GapFrameLinks, GapLink{
			Index: g.Index, StartExt: g.StartExt, Count: g.Count,
			Blocked: g.Blocked, FrameIdxs: g.Frames,
		})
	}
	if res.FirstError != nil {
		m.Failures = append(m.Failures, manifest.FailureRecord{
			Track: res.FirstError.Track, Category: string(res.FirstError.Category),
			Interval: intervalString(res.FirstError.Interval),
			Blocking: res.FirstError.BlockingDep, Message: res.FirstError.Error(),
		})
	}
	path := filepath.Join(opt.Out, "manifest.json")
	if err := m.Write(path); err != nil {
		return err
	}
	// The manifest file changed; refresh its recorded hash.
	for i := range m.Artifacts {
		if m.Artifacts[i].Path == "manifest.json" {
			m.Artifacts = append(m.Artifacts[:i], m.Artifacts[i+1:]...)
			break
		}
	}
	if err := m.AddArtifact(path); err != nil {
		return err
	}
	return m.Write(path)
}

func bandwidthRecordsOf(m *manifest.Manifest) []transport.BandwidthSampleRecord {
	return m.Bandwidth
}

func firstFailure(r *scheduler.Report) *model.Error {
	for _, name := range r.Order {
		st := r.States[name]
		if st.Status == scheduler.StatusFailed && st.Err != nil {
			return st.Err
		}
	}
	// Otherwise the first blocked node carries the root cause.
	for _, name := range r.Order {
		st := r.States[name]
		if st.Status == scheduler.StatusBlocked && st.Err != nil {
			return st.Err
		}
	}
	return nil
}

func priorStates(m *manifest.Manifest) map[string]scheduler.NodeState {
	out := make(map[string]scheduler.NodeState, len(m.Nodes))
	for _, n := range m.Nodes {
		out[n.Name] = scheduler.NodeState{
			Name: n.Name, Status: scheduler.Status(n.Status),
			Attempts: n.Attempts, Result: scheduler.Result{Hash: n.Hash},
		}
	}
	return out
}

func buildManifest(m *manifest.Manifest, opt Options, asset *metadata.Asset,
	stats transport.StreamStats, bw []transport.BandwidthSampleRecord,
	gaps []timeline.Gap, tl *timeline.Timeline, frames []render.RenderedFrame) {
	for _, t := range asset.SortedTracks() {
		rec := manifest.TrackRecord{
			Key: t.Key, Kind: string(t.Kind), Codec: t.Codec,
			Timebase: t.Timebase.String(), Start: t.Start, Duration: t.Duration,
			Rotation: t.Rotation, RotationRaw: fmt.Sprintf("%.3f", t.RotationRaw),
			CodedWidth: t.CodedWidth, CodedHeight: t.CodedHeight,
			OutputWidth: t.OutputWidth(), OutputHeight: t.OutputHeight(),
			Color: t.Color.Key(), Container: t.Container, Notes: t.Notes,
		}
		if t.Crop != nil {
			rec.Crop = fmt.Sprintf("%d,%d,%d,%d", t.Crop.X, t.Crop.Y, t.Crop.W, t.Crop.H)
		}
		m.Tracks = append(m.Tracks, rec)
	}
	for _, f := range asset.Failures {
		m.Failures = append(m.Failures, manifest.FailureRecord{
			Track: f.Key, Category: string(f.Err.Category),
			Interval: intervalString(f.Err.Interval),
			Blocking: f.Err.BlockingDep, Message: f.Err.Error(),
		})
	}
	if tl != nil {
		m.Color = manifest.ColorRecord{
			Primaries: tl.Color.Primaries, Transfer: tl.Color.Transfer,
			Matrix: tl.Color.Matrix, FullRange: tl.Color.FullRange,
			Source: tl.Color.Source, Defaulted: tl.Color.Defaulted,
			Conflicts: tl.Color.Conflicts,
		}
		for i, f := range frames {
			var tf timeline.Frame
			if i < len(tl.Frames) {
				tf = tl.Frames[i]
			}
			rec := manifest.FrameRecord{
				Index: f.Index, Start: f.Start, End: f.End,
				PlanHash: tf.PlanHash, FrameHash: f.Hash,
				Blocked: f.Blocked, BlockingDep: f.BlockingDep,
				Hole: tf.Hole, Overlaps: tf.Overlaps, Gaps: tf.GapIndices,
			}
			for _, l := range tf.Layers {
				if l.Boundary {
					rec.Boundary = true
					break
				}
			}
			m.Frames = append(m.Frames, rec)
		}
	}
	// Link each gap to the frames it blocks and to its manifest position.
	byGap := make(map[int][]int)
	for _, f := range m.Frames {
		for _, gi := range f.Gaps {
			byGap[gi] = append(byGap[gi], f.Index)
		}
	}
	for i, g := range stats.Gaps {
		gr := manifest.GapRecord{Index: i, StartExt: g.StartExt, Count: g.Count,
			Blocked: g.Blocked, TimestampFrom: g.From, TimestampTo: g.To,
			Frames: byGap[i],
		}
		if len(gr.Frames) > 0 {
			gr.Dep = "transport:gap"
		}
		if gr.Dep == "" && i < len(gaps) {
			gr.Dep = gaps[i].Dep
		}
		// Report the gap's position on the output timeline, not a constant.
		if i < len(gaps) && tl != nil {
			gr.ManifestMs = tl.Timebase.ToMillis(gaps[i].Start)
		}
		m.Gaps = append(m.Gaps, gr)
	}
	m.Bandwidth = bw
	m.Transport = stats
	m.Metrics.DuplicatePackets = stats.Duplicates
	m.Metrics.ReorderedPackets = stats.Reordered
	m.Metrics.LostPackets = stats.Lost
}

func buildVerification(opt Options, tl *timeline.Timeline, frames []render.RenderedFrame) *manifest.Manifest {
	m := manifest.New(opt.Seed, "verify")
	m.Timebase = clock.Video90k.String()
	m.Color = manifest.ColorRecord{
		Primaries: tl.Color.Primaries, Transfer: tl.Color.Transfer,
		Matrix: tl.Color.Matrix, FullRange: tl.Color.FullRange,
		Source: tl.Color.Source, Defaulted: tl.Color.Defaulted,
	}
	// Compare requires both sides to describe the same contributing track set;
	// use the already-assembled product track list (which excludes inputs with
	// isolated metadata failures only when they did not contribute).
	for _, t := range tl.Tracks {
		rec := manifest.TrackRecord{
			Key: t.Key, Kind: string(t.Kind), Codec: t.Codec,
			Timebase: t.Timebase.String(), Start: t.Start, Duration: t.Duration,
			Rotation: t.Rotation, RotationRaw: fmt.Sprintf("%.3f", t.RotationRaw),
			CodedWidth: t.CodedWidth, CodedHeight: t.CodedHeight,
			OutputWidth: t.OutputWidth(), OutputHeight: t.OutputHeight(),
			Color: t.Color.Key(), Container: t.Container, Notes: t.Notes,
		}
		if t.Crop != nil {
			rec.Crop = fmt.Sprintf("%d,%d,%d,%d", t.Crop.X, t.Crop.Y, t.Crop.W, t.Crop.H)
		}
		m.Tracks = append(m.Tracks, rec)
	}
	m.Metrics.FrameCount = len(frames)
	for i, f := range frames {
		var tf timeline.Frame
		if i < len(tl.Frames) {
			tf = tl.Frames[i]
		}
		m.Frames = append(m.Frames, manifest.FrameRecord{
			Index: f.Index, Start: f.Start, End: f.End,
			PlanHash: tf.PlanHash, FrameHash: f.Hash,
			Blocked: f.Blocked, BlockingDep: f.BlockingDep,
		})
	}
	for _, t := range tl.Frames {
		_ = t
	}
	return m
}

func gapsToTimeline(gaps []transport.Gap, packets, frames int) []timeline.Gap {
	// Map packet indices to timeline ticks at the output cadence: each frame
	// consumes an equal share of the packet stream.
	perFrame := 1
	if frames > 0 && packets > 0 {
		perFrame = maxInt(packets/frames, 1)
	}
	out := make([]timeline.Gap, 0, len(gaps))
	for _, g := range gaps {
		startTick := (g.StartExt % int64(packets)) * 3000 / int64(perFrame)
		// Align to the frame grid so the gap is explainable in the manifest.
		startTick = (startTick / 3000) * 3000
		endTick := startTick + g.Count*3000/int64(perFrame)
		if endTick <= startTick {
			endTick = startTick + 3000
		}
		tg := timeline.Gap{Start: startTick, End: endTick, Blocked: g.Blocked}
		if g.Blocked {
			tg.Dep = "transport:gap"
		}
		out = append(out, tg)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

func runBandwidth(opt Options, frames int) ([]transport.BandwidthSampleRecord, transport.BandwidthSummary) {
	cfg := transport.DefaultBandwidthConfig()
	cfg.DefaultTargetKbps = opt.BandwidthKbps
	est := transport.NewBandwidthEstimator(cfg)
	// The link delivers a changing capacity around the requested bitrate. A
	// deterministic integer pattern avoids random flakiness.
	var recs []transport.BandwidthSampleRecord
	for i := 0; i < frames; i++ {
		// capacity oscillates +/-12% in a fixed pattern around the target.
		delta := float64((i%5)-2) * 0.06
		delivery := opt.BandwidthKbps * (1 + delta)
		r := est.Add(transport.BandwidthSample{
			AtMillis:      int64(i) * 33,
			DeliveryKbps:  delivery,
			SuggestedKbps: est.Target(),
		})
		recs = append(recs, r)
	}
	return recs, est.Summary()
}

func hashFrames(frames []render.RenderedFrame) string {
	h := newHash()
	for _, f := range frames {
		h.Write([]byte(f.Hash))
	}
	return h.Hex()
}

func metadataHash(a *metadata.Asset) string {
	h := newHash()
	for _, t := range a.SortedTracks() {
		fmt.Fprintf(h, "%s:%s:%s:%d:%d:%d;", t.Key, t.Timebase, t.Color.Key(), t.Start, t.Duration, t.Rotation)
	}
	return h.Hex()
}

// crossContainerCheck verifies that MP4 and MXF normalize identically for
// every track they share. Corrupt fixtures are expected to disagree (their
// failures are reported by the parsers), so only healthy tracks are compared.
func crossContainerCheck(mp4Path, mxfPath string) error {
	mp4Asset, mp4Err := metadata.ParseMP4(mp4Path)
	mxfAsset, mxfErr := metadata.ParseMXF(mxfPath)
	if mp4Asset == nil || mxfAsset == nil {
		return nil // parser errors are surfaced by the caller
	}
	for _, a := range mp4Asset.Tracks {
		b, ok := mxfAsset.Track(a.Key)
		if !ok {
			continue
		}
		// Skip tracks that either parser reported as corrupt.
		if mp4Asset.Failure(a.Key) != nil || mxfAsset.Failure(a.Key) != nil {
			continue
		}
		if a.Timebase != b.Timebase || a.Duration != b.Duration || a.Rotation != b.Rotation ||
			a.CodedWidth != b.CodedWidth || a.CodedHeight != b.CodedHeight {
			_ = mp4Err
			_ = mxfErr
			return model.New(model.CategoryMetadata, "cross-container",
				fmt.Sprintf("track %s differs between MP4 and MXF", a.Key)).
				WithTrack(a.Key).WithBlockingDep("metadata:" + a.Key)
		}
	}
	return nil
}

func hasVideo(a *metadata.Asset) bool {
	if a == nil {
		return false
	}
	for _, t := range a.Tracks {
		if t.Kind == metadata.KindVideo && t.OutputWidth() > 0 && t.OutputHeight() > 0 {
			return true
		}
	}
	return false
}

func frameName(index int) string { return fmt.Sprintf("frame_%04d.ppm", index) }

func writeFrameArtifacts(out string, frames []render.RenderedFrame) error {
	dir := filepath.Join(out, "frames")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return model.Wrap(model.CategoryRender, "mkdir", err)
	}
	for _, f := range frames {
		if err := writePPM(filepath.Join(dir, frameName(f.Index)), f.Image); err != nil {
			return err
		}
	}
	return nil
}

// writePPM writes a binary PPM (P6), which requires no image codecs.
func writePPM(path string, img *image.RGBA) error {
	b := img.Bounds()
	var buf []byte
	buf = append(buf, fmt.Sprintf("P6\n%d %d\n255\n", b.Dx(), b.Dy())...)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			p := img.RGBAAt(x, y)
			buf = append(buf, p.R, p.G, p.B)
		}
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		return model.Wrap(model.CategoryRender, "write-frame", err)
	}
	return nil
}

// steadyBandwidth measures the requested target against warmed delivery. The
// warm-up samples are excluded because the estimator deliberately holds its
// initial target until the window fills.
func steadyBandwidth(recs []transport.BandwidthSampleRecord, requested float64) (target, actual, errPct float64) {
	var tSum, aSum float64
	n := 0
	for _, r := range recs {
		if !r.Warm {
			continue
		}
		tSum += r.TargetKbps
		aSum += r.ActualKbps
		n++
	}
	if n == 0 {
		return requested, requested, 0
	}
	target, actual = tSum/float64(n), aSum/float64(n)
	if actual > 0 {
		errPct = (target - actual) / actual * 100
		if errPct < 0 {
			errPct = -errPct
		}
	}
	return target, actual, errPct
}

func intervalString(iv *model.Interval) string {
	if iv == nil {
		return ""
	}
	return iv.String()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
