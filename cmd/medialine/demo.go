package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"medialine/internal/align"
	"medialine/internal/fault"
	"medialine/internal/index"
	"medialine/internal/manifest"
	"medialine/internal/model"
	"medialine/internal/mp4"
	"medialine/internal/pipeline"
	"medialine/internal/testkit"
	"medialine/internal/verify"
)

// runDemo builds fixtures, runs the pipeline and prints the acceptance fields.
func runDemo(parent context.Context, args []string) int {
	cfg, err := parseDemoFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "medialine demo: %v\n", err)
		return exitFatal
	}
	res, err := demo(parent, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "medialine demo: %v\n", err)
		return exitFatal
	}
	res.print(os.Stdout)
	if res.damaged {
		return exitPartial
	}
	return exitOK
}

// demoResult carries every acceptance metric.
type demoResult struct {
	RoundTripRate     float64
	KeyframeHitRate   float64
	SegmentMatchNum   int
	SegmentMatchDen   int
	AlignedBoundary   int64
	PeakInflight      int
	InflightLimit     int
	RecoveredSegments int
	Aborted           bool
	Completed         int
	Total             int
	Damaged           bool
	Canceled          bool
	Issues            int
	Elapsed           time.Duration
	Out               string
	damaged           bool
}

func (r demoResult) print(w *os.File) {
	fmt.Fprintf(w, "roundtrip_rate=%.3f\n", r.RoundTripRate)
	fmt.Fprintf(w, "keyframe_hit_rate=%.3f\n", r.KeyframeHitRate)
	fmt.Fprintf(w, "segment_hash_match=%d/%d\n", r.SegmentMatchNum, r.SegmentMatchDen)
	fmt.Fprintf(w, "aligned_boundary_us=%d\n", r.AlignedBoundary)
	fmt.Fprintf(w, "peak_inflight=%d (limit=%d)\n", r.PeakInflight, r.InflightLimit)
	fmt.Fprintf(w, "recovered_segments=%d\n", r.RecoveredSegments)
	fmt.Fprintf(w, "completed_segments=%d total_segments=%d\n", r.Completed, r.Total)
	if r.Aborted {
		fmt.Fprintln(w, "status=recovered")
	}
	if r.Damaged {
		fmt.Fprintln(w, "status=partial")
	}
	fmt.Fprintf(w, "manifest=%s\n", filepath.Join(r.Out, "manifest.json"))
	fmt.Fprintf(w, "elapsed=%s\n", formatDuration(r.Elapsed))
}

// representation is one ladder rung plus its source file and index.
type representation struct {
	alignRep  align.Representation
	model     manifest.Representation
	clip      *testkit.Clip
	track     *model.Track
	trackCuts map[int]*model.Track
}

// demo is the testable body of the demo subcommand.
func demo(parent context.Context, cfg demoConfig) (*demoResult, error) {
	started := time.Now()
	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		return nil, fmt.Errorf("create output: %w", err)
	}
	srcDir := filepath.Join(cfg.Out, "src")
	canceller := pipeline.NewCanceller(parent)
	defer canceller.Cancel(pipeline.ReasonClose)

	reps, tracks, idx, spanUS, issues, damaged, err := buildMaterial(cfg, srcDir, canceller)
	if err != nil {
		return nil, err
	}
	res := &demoResult{Out: cfg.Out, AlignedBoundary: 0, InflightLimit: cfg.Queue, Damaged: damaged, Issues: len(issues)}
	res.damaged = damaged

	// Align the ladder.
	alignReps := make([]align.Representation, 0, len(reps))
	for _, r := range reps {
		alignReps = append(alignReps, r.alignRep)
	}
	al, err := align.Align(alignReps, spanUS, align.Config{
		SegmentUS:  cfg.SegmentUS,
		SnapBack:   true,
		EditPoints: cfg.Cuts,
	})
	if err != nil {
		return nil, err
	}
	res.AlignedBoundary = al.BoundaryDelta(representationIDs(reps), idx)

	// Build transitions from the cut plan: one crossfade in the middle of the
	// timeline that links the two lowest-numbered video tracks.
	transitions := planTransitions(tracks, spanUS, cfg.SegmentUS)

	mb, err := manifest.NewBuilder(manifest.Options{SegmentUS: cfg.SegmentUS, SnapBack: true, OutDir: cfg.Out, Ext: "m4s"})
	if err != nil {
		return nil, err
	}
	for _, r := range reps {
		if err := mb.AddRepresentation(r.model); err != nil {
			return nil, err
		}
		mb.AddIssue(fmt.Sprintf("representation %s uses %s", r.model.ID, r.clip.Path))
	}

	entries, err := planEntries(mb, al, reps, tracks, transitions)
	if err != nil {
		return nil, err
	}
	res.Total = len(entries)

	// Execute the plan through the bounded pool. A cancellation aborts the
	// first pass but leaves a resumable session behind.
	run, err := executePlan(canceller, cfg, entries, reps, mb)
	if err != nil {
		return nil, err
	}
	res.PeakInflight = run.stats.Peak
	res.InflightLimit = run.stats.Limit()
	res.Canceled = run.canceled
	res.Aborted = run.canceled

	firstPass := mb.Build(spanUS, transitionRecords(transitions, reps))
	res.Completed = countComplete(firstPass.Entries)

	if res.Aborted {
		// Persist the partial manifest so operators can see exactly which
		// fragments are safe to re-run.
		if _, err := manifest.Write(firstPass, cfg.Out); err != nil {
			return nil, err
		}
		// Resume: run the plan again over the unfinished fragments. The merge
		// step deduplicates by segment ID, so fragments completed before the
		// cancellation are counted exactly once.
		resumed, rerr := resumePlan(cfg, entries, reps, mb, firstPass)
		if rerr != nil {
			return nil, rerr
		}
		res.RecoveredSegments = resumed
		res.Canceled = false
	}

	// Build and write the final manifest, valid records only.
	m := mb.Build(spanUS, transitionRecords(transitions, reps))
	res.Completed = countComplete(m.Entries)
	if len(m.Failures) > 0 {
		// Fragments that failed integrity checking count as damaged input:
		// the run produced usable output but cannot claim a clean delivery.
		res.Damaged = true
		res.damaged = true
	}
	if _, err := manifest.Write(m, cfg.Out); err != nil {
		return nil, err
	}

	// Verify what was delivered.
	vr, err := verify.Run(cfg.Out, mp4.Options{})
	if err != nil {
		return nil, err
	}
	// The numerator counts fragments that were completed and hash-verified;
	// the denominator is every planned fragment, so the ratio shows how much
	// of the batch is deliverable.
	res.SegmentMatchNum = vr.Verified
	res.SegmentMatchDen = res.Total
	if vr.Completed == 0 {
		res.RoundTripRate = 1
	} else {
		res.RoundTripRate = float64(vr.RoundTripped) / float64(vr.Completed)
	}
	if !res.Aborted {
		res.RecoveredSegments = recoveredCount(vr)
	}
	res.KeyframeHitRate = keyframeHitRate(entries, reps)
	res.Elapsed = time.Since(started)
	return res, nil
}

// buildMaterial generates source clips, parses them and builds indexes.
func buildMaterial(cfg demoConfig, srcDir string, canceller *pipeline.Canceller) ([]representation, map[int]*model.Track, map[string]*index.TrackIndex, int64, []*fault.Error, bool, error) {
	var reps []representation
	tracks := make(map[int]*model.Track)
	idx := make(map[string]*index.TrackIndex)
	var issues []*fault.Error
	damaged := false
	spanUS := int64(0)
	trackID := 0

	// The first two tracks are the two "camera" sources that transitions link;
	// the remaining tracks carry supporting video/audio.
	for b := 0; b < cfg.Bitrates; b++ {
		repID := fmt.Sprintf("bitrate%d", 6_000_000/(b+1))
		bitrate := int64(6_000_000 / (b + 1))
		spec := testkit.ClipSpec{Name: repID, Seed: cfg.Seed + int64(b)}
		for t := 0; t < cfg.Tracks; t++ {
			trackID++
			id := trackID
			if t%3 == cfg.Tracks-1 && cfg.Tracks > 1 {
				spec.Tracks = append(spec.Tracks, testkit.DefaultAudioTrack(id, cfg.DurationUS))
				continue
			}
			gop := 15 + t*5
			spec.Tracks = append(spec.Tracks, testkit.DefaultVideoTrack(id, cfg.DurationUS, gop))
		}
		clip, err := testkit.GenerateClip(srcDir, spec)
		if err != nil {
			return nil, nil, nil, 0, nil, false, fault.From(err, fault.StageParse, fault.KindCorruptSample)
		}
		if err := applyDamage(cfg.Corrupt, clip); err != nil {
			return nil, nil, nil, 0, nil, false, err
		}
		mf, err := mp4.Parse(clip.Path, mp4.Options{CheckSampleMarker: true})
		if err != nil {
			var fe *fault.Error
			if errors.As(err, &fe) && fe.Kind == fault.KindMissingIndex {
				issues = append(issues, fe)
				damaged = true
				continue
			}
			return nil, nil, nil, 0, nil, false, err
		}
		for _, is := range mf.Issues {
			issues = append(issues, is)
			damaged = true
		}
		if len(mf.Tracks) == 0 {
			issues = append(issues, fault.New(fault.StageIndex, fault.KindMissingIndex,
				"source %s has no usable tracks", clip.Path))
			damaged = true
			continue
		}
		// The representation's primary track is the first video track.
		var primary *model.Track
		trackMap := make(map[int]*model.Track, len(mf.Tracks))
		for _, tr := range mf.Tracks {
			trackMap[tr.ID] = tr
			tracks[tr.ID] = tr
			if primary == nil && tr.Kind == model.KindVideo {
				primary = tr
			}
		}
		if primary == nil {
			primary = mf.Tracks[0]
		}
		ti, err := index.Build(primary)
		if err != nil {
			return nil, nil, nil, 0, nil, false, err
		}
		idx[repID] = ti
		reps = append(reps, representation{
			alignRep: align.Representation{ID: repID, Bitrate: bitrate, TrackID: primary.ID, Index: ti},
			model: manifest.Representation{
				ID: repID, Bitrate: bitrate, TrackID: primary.ID, Codec: primary.Codec, Timescale: primary.Timescale,
			},
			clip:      clip,
			track:     primary,
			trackCuts: trackMap,
		})
		if d := primary.DurationUS(); d > spanUS {
			spanUS = d
		}
	}
	if len(reps) == 0 {
		return nil, nil, nil, 0, issues, damaged, faultsToError(issues)
	}
	return reps, tracks, idx, spanUS, issues, damaged, nil
}

func faultsToError(issues []*fault.Error) error {
	if len(issues) == 0 {
		return fmt.Errorf("no usable representations")
	}
	return issues[0]
}

// applyDamage introduces the requested source damage.
func applyDamage(kind string, clip *testkit.Clip) error {
	switch kind {
	case "tail":
		// Losing the trailing index models a partially written delivery.
		return testkit.CorruptTail(clip.Path, 24)
	case "length":
		// An oversized declared box length must be rejected at the boundary.
		return testkit.CorruptLength(clip.Path, clip.MdatOffset, 0x7ffffff0)
	case "sample":
		return testkit.CorruptSampleBytes(clip.Path, clip.FirstSampleOffset)
	}
	return nil
}

// planTransitions creates one crossfade region per two segments in the middle
// of the timeline, linking the two lowest video track IDs.
func planTransitions(tracks map[int]*model.Track, spanUS, segmentUS int64) []model.Transition {
	var videoIDs []int
	for id, tr := range tracks {
		if tr.Kind == model.KindVideo {
			videoIDs = append(videoIDs, id)
		}
	}
	sort.Ints(videoIDs)
	if len(videoIDs) < 2 {
		return nil
	}
	mid := spanUS / 2
	start := (mid / segmentUS) * segmentUS
	return []model.Transition{{
		StartUS: start,
		EndUS:   start + segmentUS/2,
		Kind:    model.TransitionCross,
		Sources: []int{videoIDs[0], videoIDs[1]},
	}}
}

// transitionRecords renders transitions for the manifest.
func transitionRecords(trs []model.Transition, reps []representation) []manifest.TransitionRecord {
	out := make([]manifest.TransitionRecord, 0, len(trs))
	for _, tr := range trs {
		rep := ""
		if len(reps) > 0 {
			rep = reps[0].model.ID
		}
		out = append(out, manifest.TransitionRecord{
			StartUS: tr.StartUS, EndUS: tr.EndUS, Kind: string(tr.Kind),
			Sources: tr.Sources, Representative: rep,
		})
	}
	return out
}

func representationIDs(reps []representation) []string {
	out := make([]string, 0, len(reps))
	for _, r := range reps {
		out = append(out, r.model.ID)
	}
	sort.Strings(out)
	return out
}

func planEntries(mb *manifest.Builder, al *align.Aligned, reps []representation, tracks map[int]*model.Track, transitions []model.Transition) ([]manifest.Entry, error) {
	alignReps := make([]align.Representation, 0, len(reps))
	for _, r := range reps {
		alignReps = append(alignReps, r.alignRep)
	}
	simple, composite, err := mb.AlignEntries(al, alignReps, tracks, transitions)
	if err != nil {
		return nil, err
	}
	return append(simple, composite...), nil
}

// execution is the result of running the plan through the pool.
type execution struct {
	stats    pipeline.Stats
	canceled bool
}

// executePlan schedules every entry, writes its fragment and records the
// outcome. Each completed entry is appended to the manifest builder only when
// its file was written and hashed successfully.
func executePlan(canceller *pipeline.Canceller, cfg demoConfig, entries []manifest.Entry, reps []representation, mb *manifest.Builder) (execution, error) {
	byRep := make(map[string]representation, len(reps))
	for _, r := range reps {
		byRep[r.model.ID] = r
	}
	var mu sync.Mutex
	var completed int64
	pool, err := pipeline.New(canceller.Context(), pipeline.Config{
		Workers: cfg.Workers,
		Queue:   cfg.Queue,
		OnFault: func(e *fault.Error) {},
	})
	if err != nil {
		return execution{}, err
	}
	defer pool.Close()

	for _, e := range entries {
		e := e
		rep, ok := byRep[e.Representation]
		if !ok {
			continue
		}
		_, serr := pool.Submit(func(ctx context.Context) error {
			return writeEntry(ctx, cfg, e, rep, reps, mb, &mu, &completed)
		}, func(_ int64, jerr error) {
			if jerr != nil {
				var fe *fault.Error
				if errors.As(jerr, &fe) && !fault.IsRecoverable(fe) {
					canceller.Cancel(pipeline.ReasonFault)
				}
			}
		})
		if serr != nil {
			break
		}
		if cfg.CancelAfter > 0 && atomic.LoadInt64(&completed) >= int64(cfg.CancelAfter) {
			canceller.Cancel(pipeline.ReasonSignal)
			break
		}
	}
	pool.Close()
	stats := pool.Stats()
	return execution{stats: stats, canceled: canceller.Canceled() && canceller.Reason() == pipeline.ReasonSignal}, nil
}

// writeEntry reads the samples for one manifest row, writes the fragment and
// hashes it. Recoverable faults are recorded as manifest failures.
func writeEntry(ctx context.Context, cfg demoConfig, e manifest.Entry, rep representation, allReps []representation, mb *manifest.Builder, mu *sync.Mutex, completed *int64) error {
	if err := ctx.Err(); err != nil {
		return fault.From(err, fault.StageCancel, fault.KindCanceled).WithSegment(e.ID)
	}
	track := rep.trackCuts[e.TrackID]
	samples, err := manifest.PlanFromEntry(e, track)
	if err != nil {
		recordFailure(mb, mu, e, err)
		return nil
	}
	src, err := mp4.Parse(rep.clip.Path, mp4.Options{CheckSampleMarker: true})
	if err != nil {
		recordFailure(mb, mu, e, err)
		return nil
	}
	sr, err := src.OpenSampleReader(1 << 20)
	if err != nil {
		recordFailure(mb, mu, e, err)
		return nil
	}
	defer sr.Close()
	sr.WithTrack(e.TrackID)

	inputs := make([]string, 0, len(allReps))
	for _, r := range allReps {
		inputs = append(inputs, r.clip.Path)
	}
	plan := mp4.SegmentPlan{
		ID:        e.ID,
		TrackID:   e.TrackID,
		Kind:      e.Kind,
		Timescale: track.Timescale,
		Codec:     track.Codec,
		StartUS:   e.StartUS,
		EndUS:     e.EndUS,
		Samples:   samples,
	}
	for _, s := range samples {
		b, rerr := sr.ReadSample(s)
		if rerr != nil {
			recordFailure(mb, mu, e, rerr)
			return nil
		}
		plan.Payloads = append(plan.Payloads, append([]byte(nil), b...))
	}
	abs, err := manifest.AbsPath(cfg.Out, e.File)
	if err != nil {
		recordFailure(mb, mu, e, err)
		return nil
	}
	if _, err := mp4.WriteSegment(abs, plan, mp4.WriterOptions{Root: cfg.Out, MaxPayload: 1 << 20, Inputs: inputs}); err != nil {
		recordFailure(mb, mu, e, err)
		return nil
	}
	hash, err := verify.HashFile(abs)
	if err != nil {
		recordFailure(mb, mu, e, err)
		return nil
	}
	e.ContentHash = hash
	e.Status = "complete"
	e.SampleNumbers = sampleNumbers(samples)
	mu.Lock()
	mb.Append(e)
	atomic.AddInt64(completed, 1)
	mu.Unlock()
	return nil
}

func sampleNumbers(samples []model.Sample) []int {
	out := make([]int, len(samples))
	for i, s := range samples {
		out[i] = s.Number
	}
	return out
}

func recordFailure(mb *manifest.Builder, mu *sync.Mutex, e manifest.Entry, err error) {
	fe, _ := fault.As(err)
	f := manifest.Failure{ID: e.ID, Representation: e.Representation, TrackID: e.TrackID, StartUS: e.StartUS, EndUS: e.EndUS, File: e.File}
	if fe != nil {
		f.Stage = string(fe.Stage)
		f.Kind = string(fe.Kind)
		f.Error = fe.Error()
	} else if err != nil {
		f.Error = err.Error()
	}
	mu.Lock()
	mb.AddFailure(f)
	mu.Unlock()
}

func countComplete(entries []manifest.Entry) int {
	var n int
	for _, e := range entries {
		if e.Status == "complete" {
			n++
		}
	}
	return n
}

func recoveredCount(vr *verify.Report) int {
	var n int
	for _, r := range vr.Results {
		if r.OK {
			n++
		}
	}
	return n
}

// keyframeHitRate is the share of committed boundaries that landed on a
// keyframe in every representation.
func keyframeHitRate(entries []manifest.Entry, reps []representation) float64 {
	if len(entries) == 0 {
		return 1
	}
	idxByRep := make(map[string]*index.TrackIndex, len(reps))
	for _, r := range reps {
		idxByRep[r.model.ID] = r.alignRep.Index
	}
	hits, total := 0, 0
	seen := map[int64]bool{}
	for _, e := range entries {
		if seen[e.StartUS] {
			continue
		}
		seen[e.StartUS] = true
		total++
		allHit := true
		for _, r := range reps {
			if _, ok := idxByRep[r.model.ID].Exact(e.StartUS); !ok {
				allHit = false
				break
			}
		}
		if allHit {
			hits++
		}
	}
	if total == 0 {
		return 1
	}
	return float64(hits) / float64(total)
}

// resumePlan re-runs every fragment that the aborted pass did not deliver.
// Fragments already recorded in the partial manifest are skipped so a resumed
// run never double-counts completed work; the remaining work is merged through
// the same deterministic ordering as the first pass.
func resumePlan(cfg demoConfig, entries []manifest.Entry, reps []representation, mb *manifest.Builder, partial *manifest.Manifest) (int, error) {
	doneIDs := make(map[string]bool, len(partial.Entries))
	for _, e := range partial.Entries {
		if e.Status == "complete" {
			doneIDs[e.ID] = true
		}
	}
	failedIDs := make(map[string]bool, len(partial.Failures))
	for _, f := range partial.Failures {
		failedIDs[f.ID] = true
	}
	var pending []manifest.Entry
	for _, e := range entries {
		if doneIDs[e.ID] || failedIDs[e.ID] {
			continue
		}
		pending = append(pending, e)
	}
	if len(pending) == 0 {
		return len(doneIDs), nil
	}

	canceller := pipeline.NewCanceller(context.Background())
	defer canceller.Cancel(pipeline.ReasonClose)
	var mu sync.Mutex
	var completed int64
	pool, err := pipeline.New(canceller.Context(), pipeline.Config{Workers: cfg.Workers, Queue: cfg.Queue})
	if err != nil {
		return 0, err
	}
	for _, e := range pending {
		e := e
		rep, ok := repByID(reps, e.Representation)
		if !ok {
			continue
		}
		if _, err := pool.Submit(func(ctx context.Context) error {
			return writeEntry(ctx, cfg, e, rep, reps, mb, &mu, &completed)
		}, nil); err != nil {
			break
		}
	}
	pool.Close()

	// Count each completed fragment exactly once, from the merged manifest.
	final := mb.Build(partial.SpanUS, partial.Transitions)
	seen := make(map[string]bool, len(final.Entries))
	for _, e := range final.Entries {
		if e.Status == "complete" {
			seen[e.ID] = true
		}
	}
	return len(seen), nil
}

func repByID(reps []representation, id string) (representation, bool) {
	for _, r := range reps {
		if r.model.ID == id {
			return r, true
		}
	}
	return representation{}, false
}
