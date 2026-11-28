package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"avpipe/internal/audit"
	"avpipe/internal/clock"
	"avpipe/internal/demux"
	"avpipe/internal/media"
	"avpipe/internal/merge"
	"avpipe/internal/remux"
	"avpipe/internal/worker"
)

// runConfig is one end-to-end conversion.
type runConfig struct {
	Inputs   []string
	Output   string
	Manifest string
	Format   string
	Segment  media.Rational
	Workers  int
	Queue    int
	Cache    int
	WrapBits int
	// AllowQuarantine quarantines missing timestamps and DTS regressions
	// instead of failing the run.
	AllowQuarantine bool
	// AllowUnmatchedTracks lets inputs contribute only their mapped tracks.
	AllowUnmatchedTracks bool
	// CancelAfter cancels the run after this many packets, used by the
	// selfcheck cancellation scenario. Zero disables it.
	CancelAfter int64
	// OnProgress, when set, is called periodically with the packet count.
	OnProgress func(n int64)
}

// runResult summarises a conversion.
type runResult struct {
	ManifestPath  string
	Output        string
	Segments      int
	Packets       int64
	Bytes         int64
	WrapRecovered int
	Quarantine    int
	QueuePeak     int
	Canceled      bool
	TrackIDs      []string
	Samples       []string
}

// trackPlan binds one output track to the input and source track it comes from.
type trackPlan struct {
	input  *demux.Input
	source media.Track // original, with the source-side ID
	outID  string      // ID used in worker/merge/remux
	out    media.Track // copy of source with outID
}

// run executes the full pipeline. It is the single place where cancellation
// and temporary-file cleanup are coordinated: if any stage fails or the
// context is canceled, no output file and no manifest are published.
func run(ctx context.Context, cfg runConfig) (*runResult, error) {
	if err := validateRunConfig(cfg); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var inputs []*demux.Input
	defer func() {
		for _, in := range inputs {
			in.Close()
		}
	}()

	var plans []*trackPlan
	wrapRecovered := 0
	for _, path := range cfg.Inputs {
		in, err := demux.Open(path, demux.Options{Limits: demux.DefaultLimits(), WrapBits: cfg.WrapBits})
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, in)
		m, err := in.Mapping(nil, cfg.AllowUnmatchedTracks)
		if err != nil {
			return nil, asMediaErr(mappingErr(err, path), "mapping")
		}
		// Plans follow source stream order, not the ID-sorted mapping order, so
		// the output container keeps the same stream indices as the input.
		// Stable track IDs ("v0", "a1") are derived from those indices, which
		// is what lets verify re-demux the output and compare it to the
		// manifest segment by segment.
		for _, src := range in.Tracks {
			if _, ok := m.BySource[src.Source]; !ok {
				continue
			}
			outID := src.ID
			if len(cfg.Inputs) > 1 {
				outID = fmt.Sprintf("%s:%s", filepath.Base(path), src.ID)
			}
			out := src
			out.ID = outID
			plans = append(plans, &trackPlan{input: in, source: src, outID: outID, out: out})
		}
	}
	if len(plans) == 0 {
		return nil, media.Errorf(media.KindPolicy, media.Origin{}, "run",
			"no tracks selected from %d input(s)", len(cfg.Inputs))
	}

	// Index plans by input so the feeder knows which source tracks belong to
	// which output ID.
	planByInput := map[*demux.Input]map[string]*trackPlan{}
	for _, p := range plans {
		m := planByInput[p.input]
		if m == nil {
			m = map[string]*trackPlan{}
			planByInput[p.input] = m
		}
		m[p.source.ID] = p
	}

	merger := merge.New(merge.Policy{
		AllowMissingTimestamp: cfg.AllowQuarantine,
		AllowDTSRegression:    cfg.AllowQuarantine,
		MaxPackets:            4_000_000,
	})

	pool, err := worker.New(ctx, worker.Config{Workers: cfg.Workers, Queue: cfg.Queue, Cache: cfg.Cache},
		func(ctx context.Context, p media.Packet) (media.Packet, error) {
			if len(p.Data) == 0 {
				return media.Packet{}, media.Errorf(media.KindCorrupt, p.Origin,
					"worker.normalize", "empty payload")
			}
			return p.Clone(), nil
		})
	if err != nil {
		return nil, asMediaErr(err, "worker")
	}

	var feedErr error
	var fed int64
	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		// Close must run even when feeding fails: it is what closes the
		// results stream the drain goroutine and main are waiting on.
		// Close is idempotent, so the success path is unaffected.
		defer pool.Close()
		for _, in := range inputs {
			if err := feedInput(ctx, in, planByInput[in], pool, cfg, &fed); err != nil {
				feedErr = err
				return
			}
		}
	}()

	var mergeErr error
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for p := range pool.Results() {
			if _, err := merger.Add(p); err != nil {
				mergeErr = err
				pool.Cancel()
				for range pool.Results() {
				}
				return
			}
			if cfg.OnProgress != nil && fed > 0 && fed%512 == 0 {
				cfg.OnProgress(fed)
			}
		}
	}()
	<-feedDone
	<-drainDone

	// A worker or merge failure cancels the pool internally, which makes the
	// feeder observe a cancellation. The underlying failure must win over that
	// synthetic cancel, otherwise a corrupt input would be reported as an
	// operator cancellation. Only a cancellation from the parent context, or a
	// cancel budget deliberately reached by the feeder, is a real cancel.
	operatorCanceled := ctx.Err() != nil || cfg.CancelAfter > 0
	if mergeErr != nil && !isCancelKind(mergeErr) {
		return nil, asMediaErr(mergeErr, "merge")
	}
	if err := pool.Err(); err != nil && !isCancelKind(err) {
		return nil, asMediaErr(err, "worker")
	}
	if feedErr != nil && !isCancelKind(feedErr) {
		return nil, feedErr
	}
	if operatorCanceled {
		return &runResult{Canceled: true, QueuePeak: pool.Stats().QueuePeak},
			media.New(media.KindCancel, media.Origin{Stage: "run"}, "run", "canceled before output", ctx.Err())
	}
	if mergeErr != nil {
		return nil, asMediaErr(mergeErr, "merge")
	}
	if err := pool.Err(); err != nil {
		return nil, asMediaErr(err, "worker")
	}
	if feedErr != nil {
		return nil, feedErr
	}

	ordered, err := merger.Sorted()
	if err != nil {
		return nil, asMediaErr(err, "merge")
	}
	if err := merge.ValidateOrder(ordered); err != nil {
		return nil, asMediaErr(err, "merge.validate")
	}

	// --- Build and write the output container. ---
	videoTracks := map[string]bool{}
	remuxCfg := remux.Config{Format: cfg.Format, Path: cfg.Output,
		MuxerOptions: muxerOptions(plans, cfg.Format)}
	for _, p := range plans {
		if p.out.Kind == media.TrackVideo {
			videoTracks[p.outID] = true
		}
		remuxCfg.Tracks = append(remuxCfg.Tracks, remux.TrackSpec{Track: p.out})
	}
	w, err := remux.Open(ctx, remuxCfg)
	if err != nil {
		return nil, asMediaErr(err, "remux.open")
	}
	defer w.Abort()
	for _, p := range plans {
		cp, ok := p.input.CodecParameters(p.source.ID)
		if !ok {
			return nil, media.Errorf(media.KindInternal, media.Origin{TrackID: p.outID},
				"run.remux", "codec parameters missing for %s", p.outID)
		}
		if err := w.AcceptParameters(p.outID, cp); err != nil {
			return nil, asMediaErr(err, "remux.params")
		}
	}
	if err := w.Start(ctx); err != nil {
		return nil, asMediaErr(err, "remux.start")
	}
	// The muxer can renumber or re-order packets as it interleaves chunks, so
	// the audit hashes the canonical order produced by the merge stage, not
	// the on-disk order. Verification re-sorts the demuxed output with the
	// same comparator before segmenting.
	media.SortPresentation(ordered)
	var written int64
	for _, p := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, media.New(media.KindCancel, media.Origin{}, "run.write", "canceled during output", err)
		}
		if err := w.WritePacket(p); err != nil {
			return nil, asMediaErr(err, "remux.write")
		}
		written++
	}
	if err := w.Commit(ctx); err != nil {
		return nil, asMediaErr(err, "remux.commit")
	}

	// --- Audit. ---
	segSrc, groups := audit.Segmenter(cfg.Segment, ordered, videoTracks)
	// Drift between each audio track and the video reference clock is judged
	// per segment. When it exceeds tolerance the cut is recorded with the
	// reason, so an operator can see why a boundary exists. This never adds
	// or removes a boundary: the segmentation already cuts on keyframes, and
	// drift only annotates the audit.
	attachDriftReasons(segSrc, groups, videoTracks)
	records := audit.BuildSegmentRecords(segSrc, groups)
	aw, err := audit.NewWriter(cfg.Manifest, 0)
	if err != nil {
		return nil, asMediaErr(err, "audit.open")
	}
	mf := aw.Manifest()
	mf.Workers = cfg.Workers
	mf.Queue = cfg.Queue
	mf.SegmentSecs = cfg.Segment
	for _, in := range inputs {
		wrapRecovered += in.Wraps()
	}
	mf.WrapRecovered = wrapRecovered
	mf.Packets = len(ordered)
	mf.QueuePeak = pool.Stats().QueuePeak
	mf.VFRFrames = countVFR(ordered, videoTracks)
	mf.Quarantined = merger.Stats().Quarantined
	for _, in := range cfg.Inputs {
		aw.AddInput(in)
	}
	for _, p := range plans {
		aw.AddTrack(p.out)
	}
	for _, rec := range records {
		aw.AddSegment(rec)
	}
	if err := aw.Commit(); err != nil {
		return nil, asMediaErr(err, "audit.commit")
	}

	res := &runResult{
		ManifestPath: cfg.Manifest, Output: cfg.Output,
		Segments: len(records), Packets: written, Bytes: totalBytes(ordered),
		WrapRecovered: wrapRecovered, Quarantine: merger.Stats().Quarantined,
		QueuePeak: mf.QueuePeak, TrackIDs: planIDs(plans),
	}
	for i := 0; i < len(ordered) && i < 3; i++ {
		res.Samples = append(res.Samples, ordered[i].Fingerprint())
	}
	return res, nil
}

// feedInput reads one input to EOF and submits only packets for tracks that
// are mapped to an output, re-keying each to its output ID.
func feedInput(ctx context.Context, in *demux.Input, planMap map[string]*trackPlan,
	pool *worker.Pool, cfg runConfig, fed *int64) error {
	for {
		if err := ctx.Err(); err != nil {
			return media.New(media.KindCancel, media.Origin{}, "run.feed", "canceled", err)
		}
		if cfg.CancelAfter > 0 && *fed >= cfg.CancelAfter {
			pool.Cancel()
			return media.New(media.KindCancel, media.Origin{}, "run.feed", "cancel budget reached", nil)
		}
		p, err := in.Packet()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return asMediaErr(err, "demux")
		}
		plan, ok := planMap[p.TrackID]
		if !ok {
			continue
		}
		p.TrackID = plan.outID
		if err := pool.Submit(p); err != nil {
			if errors.Is(err, worker.ErrCanceled) {
				return media.New(media.KindCancel, media.Origin{}, "run.feed", "pool canceled", nil)
			}
			return asMediaErr(err, "worker.submit")
		}
		*fed++
	}
}

func validateRunConfig(cfg runConfig) error {
	if len(cfg.Inputs) == 0 {
		return media.Errorf(media.KindUsage, media.Origin{}, "run", "at least one input is required")
	}
	if cfg.Output == "" {
		return media.Errorf(media.KindUsage, media.Origin{}, "run", "output path is required")
	}
	if cfg.Manifest == "" {
		return media.Errorf(media.KindUsage, media.Origin{}, "run", "manifest path is required")
	}
	if cfg.Format != "mp4" && cfg.Format != "mxf" {
		return media.Errorf(media.KindUsage, media.Origin{}, "run", "output format must be mp4 or mxf")
	}
	if !cfg.Segment.Valid() || cfg.Segment.Num <= 0 {
		return media.Errorf(media.KindUsage, media.Origin{}, "run", "segment duration must be positive")
	}
	if cfg.Workers <= 0 {
		return media.Errorf(media.KindUsage, media.Origin{}, "run", "worker count must be positive")
	}
	if cfg.Queue <= 0 {
		return media.Errorf(media.KindUsage, media.Origin{}, "run", "queue size must be positive")
	}
	return nil
}

// muxerOptions pins container-level choices so re-demuxing the output yields
// exactly the timestamps the audit recorded.
//
// MP4 chooses a default video timescale (commonly 1/15360 for 30fps). The
// source may use another base, and the muxer would then rescale every packet,
// changing the hash verify recomputes. Pinning the video timescale to the one
// common to the selected video tracks keeps PTS/DTS bit-identical. Audio
// keeps the muxer's native rate, which for AAC is the sample rate, so it
// already matches.
func muxerOptions(plans []*trackPlan, format string) map[string]string {
	if format != "mp4" {
		return nil
	}
	var scale media.Rational
	for _, p := range plans {
		if p.out.Kind != media.TrackVideo || !p.out.TimeBase.Valid() {
			continue
		}
		// video_track_timescale is an integer frequency, so only a 1/N base
		// can be expressed.
		if p.out.TimeBase.Num != 1 {
			return nil
		}
		if scale.Num == 0 {
			scale = p.out.TimeBase
			continue
		}
		if !scale.Equal(p.out.TimeBase) {
			// Mixed timescales cannot share one value; leave the muxer's
			// native choice in place rather than silently rescaling one track
			// differently from the other.
			return nil
		}
	}
	if scale.Num == 0 {
		return nil
	}
	return map[string]string{
		"video_track_timescale": fmt.Sprintf("%d", scale.Den),
		// Preserve encoder pre-roll instead of shifting the whole timeline:
		// shifting would change every timestamp the manifest audited.
		"avoid_negative_ts": "disabled",
	}
}

// isCancelKind reports whether err is a cancellation, whether it came from the
// worker pool sentinel or from the unified error model. A worker or merge
// failure cancels the pool as a side effect; that synthetic cancel must not
// hide the real failure.
func isCancelKind(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, worker.ErrCanceled) {
		return true
	}
	if me, ok := media.AsError(err); ok && me.Kind == media.KindCancel {
		return true
	}
	return false
}

// attachDriftReasons annotates each segment with clock-drift decisions.
//
// The earliest presentation timestamp among the video tracks is the reference
// clock. For every other track, the earliest packet in the segment is the
// subject. A drift monitor observes the difference between subject and
// reference from one segment to the next; when it wanders past tolerance the
// segment records the reason. Boundaries are not moved: segmentation already
// cuts on keyframes and packet counts must stay deterministic for replay.
func attachDriftReasons(segs []audit.SegmentSource, groups [][]media.Packet, videoTracks map[string]bool) {
	if len(groups) == 0 || len(videoTracks) == 0 {
		return
	}
	monitor, err := clock.NewMonitor(clock.DefaultDriftConfig())
	if err != nil {
		return
	}
	for i, g := range groups {
		if i >= len(segs) {
			break
		}
		ref, refOK := firstTime(g, videoTracks, true)
		if !refOK {
			continue
		}
		subject, subjOK := firstTime(g, videoTracks, false)
		if !subjOK {
			continue
		}
		d, err := monitor.Observe(clock.Observation{Reference: ref, Subject: subject,
			Origin: media.Origin{Stage: "run.drift"}})
		if err != nil {
			continue
		}
		if d.Class != clock.DriftNone {
			segs[i].Reasons = append(segs[i].Reasons, d.Class.String())
		}
	}
}

// firstTime returns the earliest timestamp in the group that belongs to a
// video track (when wantVideo is true) or to a non-video track otherwise.
func firstTime(g []media.Packet, videoTracks map[string]bool, wantVideo bool) (media.Time, bool) {
	var best media.Time
	found := false
	for _, p := range g {
		if !p.HasPTS() {
			continue
		}
		if videoTracks[p.TrackID] != wantVideo {
			continue
		}
		t := media.MustTime(p.PTS, p.TimeBase)
		if !found || t.Cmp(best) < 0 {
			best = t
			found = true
		}
	}
	return best, found
}

// asMediaErr tags a plain error with the stage it occurred in, preserving an
// existing media.Error's kind so the exit code stays meaningful.
func asMediaErr(err error, stage string) error {
	if err == nil {
		return nil
	}
	if me, ok := media.AsError(err); ok {
		return me.WithStage(stage)
	}
	return media.New(media.KindInternal, media.Origin{Stage: stage}, stage, err.Error(), err)
}

// mappingErr converts a demux.MappingError into the unified error model so the
// terminal exit code reflects whether the input itself was corrupt (a track
// disabled by illegal display metadata) or the mapping rule was rejected.
func mappingErr(err error, path string) error {
	var me *demux.MappingError
	if !errors.As(err, &me) {
		return err
	}
	origin := me.Origin
	if origin.File == "" {
		origin.File = path
	}
	if origin.TrackID == "" {
		origin.TrackID = me.TrackID
	}
	// The MappingError's own message is complete, so it is used as the unified
	// error's message rather than as a cause; attaching it again as the cause
	// would duplicate the text in the rendered error.
	return media.New(me.MediaKind(), origin, "demux.Mapping", me.Message, nil)
}

// countVFR counts video packets whose inter-frame spacing differs from the
// most common spacing. Their presence proves the pipeline preserved variable
// frame rate rather than silently normalising to the average.
func countVFR(pkts []media.Packet, video map[string]bool) int {
	last := map[string]int64{}
	lastSet := map[string]bool{}
	var gaps []int64
	for _, p := range pkts {
		if !video[p.TrackID] || !p.HasPTS() {
			continue
		}
		if lastSet[p.TrackID] {
			if d := p.PTS - last[p.TrackID]; d > 0 {
				gaps = append(gaps, d)
			}
		}
		last[p.TrackID] = p.PTS
		lastSet[p.TrackID] = true
	}
	if len(gaps) == 0 {
		return 0
	}
	counts := map[int64]int{}
	for _, g := range gaps {
		counts[g]++
	}
	var best int64
	var bestN int
	for g, n := range counts {
		if n > bestN || (n == bestN && g < best) {
			best, bestN = g, n
		}
	}
	irregular := 0
	for _, g := range gaps {
		if g != best {
			irregular++
		}
	}
	return irregular
}

func totalBytes(pkts []media.Packet) int64 {
	var n int64
	for _, p := range pkts {
		n += int64(len(p.Data))
	}
	return n
}

func planIDs(plans []*trackPlan) []string {
	out := make([]string, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.outID)
	}
	sort.Strings(out)
	return out
}

var _ = clock.DefaultWrapConfig
var _ = os.Stat
var _ = strings.TrimSpace
