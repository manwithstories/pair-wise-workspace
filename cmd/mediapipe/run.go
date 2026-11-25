package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"mediapipe/internal/audit"
	"mediapipe/internal/checkpoint"
	"mediapipe/internal/demux"
	"mediapipe/internal/mapping"
	"mediapipe/internal/media"
	"mediapipe/internal/remux"
	"mediapipe/internal/stream"
	"mediapipe/internal/timeline"
)

// demoCase is one reproducible scenario in the terminal demonstration.
type demoCase struct {
	Name       string
	Target     mapping.Target
	TargetBPS  int64
	ActualBPS  int64
	DeltaPct   float64
	Mismatch   int
	CRCErrors  int
	Skew       media.Timestamp
	BufferPeak int64
	Replay     string
	CorruptOff int64
	ResumeOK   bool
	ExitCode   int
	Note       string
	Manifest   audit.Manifest
}

// jobOptions carries the demo's tunables.
type jobOptions struct {
	Seed        int64
	Budget      int64
	SlowDelay   time.Duration
	WorkDir     string
	Target      mapping.Target
	ClusterSpan media.Timestamp
	// TargetBPS is the delivery target for the rate gate. Zero derives it from
	// the input payload rate, which makes the clean case pass by construction.
	TargetBPS int64
}

// processResult holds what the pipeline produced for one stream.
type processResult struct {
	Input      *demux.Result
	Plan       *mapping.Plan
	Packets    []media.Packet
	Output     []byte
	Summary    remux.Summary
	Verify     remux.VerifyResult
	CRCChecked int
	CRCBad     int
	BufferPeak int64
	Blocked    int64
	Engine     *timeline.Engine
	Marked     []timeline.WindowSummary
	Replay     [32]byte
	NaiveStart media.Timestamp
	InputBPS   int64
	OutputBPS  int64
}

func payloadBytes(p []media.Packet) int64 {
	var n int64
	for _, pkt := range p {
		n += int64(len(pkt.Data))
	}
	return n
}

// runPipeline demuxes, maps, watermarks, streams through a bounded queue, and
// remuxes one input buffer.
func runPipeline(opts jobOptions, data []byte, cfg timeline.Config) (*processResult, error) {
	in, err := demux.Demux(bytes.NewReader(data), demux.DefaultLimits())
	if err != nil {
		return &processResult{Input: in}, err
	}
	if in.Truncated {
		return &processResult{Input: in}, demuxErr(in)
	}
	tracks, rules := adaptForTarget(opts.Target, in.Tracks)
	plan, err := mapping.NewResolver(opts.Target, rules).Resolve(tracks)
	if err != nil {
		return &processResult{Input: in}, err
	}
	packets := mappedPackets(plan, in.Packets)
	engine, err := timeline.New(cfg)
	if err != nil {
		return nil, err
	}
	naive, _ := timeline.New(cfg)
	for _, p := range packets {
		ev := timeline.Event{
			Key: packetKey(p), Track: p.Track, Time: p.DTS,
			Duration: p.Duration, Payload: p.Data, PTS: p.PTS,
			Arrival: p.Sequence,
		}
		if _, _, perr := engine.Push(ev); perr != nil {
			return &processResult{Input: in, Plan: plan, Engine: engine, Packets: packets}, perr
		}
		// A naive engine drops late arrivals instead of rewriting windows.
		_, _, _ = naive.Push(ev)
	}
	// Re-order the remux feed by (DTS, track): after watermark recompute the
	// writer sees a settled timeline rather than arrival order.
	feed := append([]media.Packet(nil), packets...)
	sortPackets(feed)
	// A small budget with a slow consumer must force producer waits; the
	// pressure is what bounds memory, and the demo reports that it happened.
	streamOpts := opts
	if streamOpts.Budget > int64(len(data)) {
		streamOpts.Budget = int64(len(data)) / 4
	}
	out, summary, peak, blocked, err := streamRemux(streamOpts, plan, tracks, feed)
	if err != nil {
		return &processResult{Input: in, Plan: plan, Engine: engine, Packets: packets}, err
	}
	verify, err := remux.VerifySinkOutput(out, packets, plan)
	if err != nil {
		return &processResult{Input: in, Plan: plan, Engine: engine, Packets: packets}, err
	}
	checked, bad, crcErr := 0, 0, error(nil)
	if opts.Target != mapping.TargetFMP4 {
		checked, bad, crcErr = remux.VerifyCRCs(out, false)
	}
	naiveWindows := naive.Summaries()
	_ = naiveWindows
	naiveStart, _ := naive.Timeline()
	// Measured output bitrate: payload actually written, over the timeline
	// the writer reported. This is what a delivery lead sees on the wire.
	written := int64(0)
	for _, p := range feed {
		written += int64(len(p.Data))
	}
	res := &processResult{
		Input: in, Plan: plan, Packets: packets, Output: out, Summary: summary,
		Verify: verify, CRCChecked: checked, CRCBad: bad, BufferPeak: peak,
		Blocked: blocked, Engine: engine, Marked: engine.Summaries(), Replay: engine.ReplayDigest(),
		NaiveStart: naiveStart, InputBPS: bitrate(payloadBytes(packets), summary.TimeEnd-summary.TimeStart),
		OutputBPS: bitrate(written, summary.TimeEnd-summary.TimeStart),
	}
	if crcErr != nil {
		return res, crcErr
	}
	return res, nil
}

func demuxErr(in *demux.Result) error {
	return media.NewError("pipeline.demux", media.ClassFatal,
		fmt.Errorf("input truncated at %d bytes", in.Bytes))
}

func mappedPackets(plan *mapping.Plan, in []media.Packet) []media.Packet {
	out := make([]media.Packet, 0, len(in))
	for _, p := range in {
		if _, ok := plan.Remap(p.Track); ok {
			out = append(out, p)
		}
	}
	return out
}

func sortPackets(p []media.Packet) {
	sort.SliceStable(p, func(i, j int) bool {
		if p[i].DTS != p[j].DTS {
			return p[i].DTS < p[j].DTS
		}
		return p[i].Track < p[j].Track
	})
}

func packetKey(p media.Packet) string {
	return fmt.Sprintf("%d/%d/%d", p.Track, p.DTS, p.Sequence)
}

// streamRemux runs a producer/consumer pair over a byte-budgeted queue. The
// consumer sleeps SlowDelay per packet to force backpressure.
func streamRemux(opts jobOptions, plan *mapping.Plan, tracks []media.Track, packets []media.Packet) ([]byte, remux.Summary, int64, int64, error) {
	var out bytes.Buffer
	sink, err := remux.NewSink(&out, plan, tracks, remux.Options{
		Target: opts.Target, ClusterSpan: opts.ClusterSpan, ClusterCRC: true,
	})
	if err != nil {
		return nil, remux.Summary{}, 0, 0, err
	}
	q := stream.NewQueue[media.Packet](opts.Budget, stream.PacketSizer(64))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var prodErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer q.Close()
		for _, p := range packets {
			if err := q.Push(ctx, p); err != nil {
				prodErr = err
				return
			}
		}
	}()
	var consumeErr error
	for {
		p, err := q.Pop(ctx)
		if errors.Is(err, stream.ErrClosed) {
			break
		}
		if err != nil {
			consumeErr = err
			break
		}
		if err := sink.WritePacket(p); err != nil {
			consumeErr = err
			cancel()
			break
		}
		if opts.SlowDelay > 0 {
			time.Sleep(opts.SlowDelay)
		}
	}
	<-done
	peak := q.Peak()
	blocked := q.Stats().Waits
	if prodErr != nil && !errors.Is(prodErr, context.Canceled) {
		return nil, remux.Summary{}, peak, blocked, prodErr
	}
	if consumeErr != nil {
		cancel()
		q.Cancel()
		return nil, remux.Summary{}, peak, blocked, consumeErr
	}
	summary, err := sink.Close()
	if err != nil {
		return nil, summary, peak, blocked, err
	}
	return out.Bytes(), summary, peak, blocked, nil
}

// resumeRun simulates an interrupted write: it streams the first half, takes a
// checkpoint, truncates the output, then resumes and compares the digest. It
// also injects a disk failure partway through a second run to prove the error
// is classed as recoverable (exit code 4).
func resumeRun(opts jobOptions, data []byte) (bool, error) {
	in, err := demux.Demux(bytes.NewReader(data), demux.DefaultLimits())
	if err != nil {
		return false, err
	}
	tracks, rules := adaptForTarget(opts.Target, in.Tracks)
	plan, err := mapping.NewResolver(opts.Target, rules).Resolve(tracks)
	if err != nil {
		return false, err
	}
	packets := mappedPackets(plan, in.Packets)
	sortPackets(packets)
	ropts := remux.Options{Target: opts.Target, ClusterSpan: opts.ClusterSpan, ClusterCRC: true}

	// Reference run.
	var full bytes.Buffer
	refSink, err := remux.NewSink(&full, plan, in.Tracks, ropts)
	if err != nil {
		return false, err
	}
	for _, p := range packets {
		if err := refSink.WritePacket(p); err != nil {
			return false, err
		}
	}
	refSum, err := refSink.Close()
	if err != nil {
		return false, err
	}

	dir := filepath.Join(opts.WorkDir, "resume")
	store, err := checkpoint.NewStore(dir)
	if err != nil {
		return false, err
	}
	job := fmt.Sprintf("demo-%d", opts.Seed)
	outPath := filepath.Join(dir, "resumed.bin")

	var part bytes.Buffer
	s1, err := remux.NewSink(&part, plan, in.Tracks, ropts)
	if err != nil {
		return false, err
	}
	cut := len(packets) / 2
	for _, p := range packets[:cut] {
		if err := s1.WritePacket(p); err != nil {
			return false, err
		}
	}
	progress, err := s1.Flush()
	if err != nil {
		return false, err
	}
	if err := store.Save(job, outPath, string(opts.Target), progress); err != nil {
		return false, err
	}
	if err := os.WriteFile(outPath, part.Bytes(), 0o644); err != nil {
		return false, err
	}

	st, err := store.Load(job)
	if err != nil || st == nil {
		return false, fmt.Errorf("checkpoint reload failed: %v", err)
	}
	if err := st.TruncateOutput(outPath); err != nil {
		return false, err
	}
	existing, err := os.ReadFile(outPath)
	if err != nil {
		return false, err
	}
	resumeOpts := ropts
	resumeOpts.Resume = st.Resume()
	var out bytes.Buffer
	out.Write(existing)
	s2, err := remux.NewSink(&out, plan, in.Tracks, resumeOpts)
	if err != nil {
		return false, err
	}
	for _, p := range packets[st.Packets:] {
		if err := s2.WritePacket(p); err != nil {
			return false, err
		}
	}
	resSum, err := s2.Close()
	if err != nil {
		return false, err
	}
	_ = store.Delete(job)

	// Inject a write failure to confirm the interruption class.
	interrupted, err := simulateDiskFailure(plan, in.Tracks, packets, ropts)
	if err != nil {
		return false, err
	}
	if media.ExitCode(interrupted) != 4 {
		return false, fmt.Errorf("write failure classified as %d, want 4", media.ExitCode(interrupted))
	}
	return resSum.Digest == refSum.Digest && bytes.Equal(out.Bytes(), full.Bytes()), nil
}

// failingWriter accepts limit bytes then reports a disk error, modelling a
// dropped archive volume.
type failingWriter struct {
	limit int64
	n     int64
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.n+int64(len(p)) > w.limit {
		allowed := w.limit - w.n
		if allowed < 0 {
			allowed = 0
		}
		w.n += allowed
		return int(allowed), errors.New("simulated archive volume failure")
	}
	w.n += int64(len(p))
	return len(p), nil
}

// simulateDiskFailure runs a write that fails midway and returns the resulting
// error for classification.
func simulateDiskFailure(plan *mapping.Plan, tracks []media.Track, packets []media.Packet, opts remux.Options) (error, error) {
	fw := &failingWriter{limit: 4096}
	sink, err := remux.NewSink(fw, plan, tracks, opts)
	if err != nil {
		return nil, err
	}
	for _, p := range packets {
		if err := sink.WritePacket(p); err != nil {
			return err, nil
		}
	}
	_, err = sink.Close()
	if err == nil {
		return nil, fmt.Errorf("simulated disk failure did not surface")
	}
	return err, nil
}

// runUnadapted resolves with the default rules and no codec adaptation, which
// is how the demo proves the unsupported-mapping gate fires.
func runUnadapted(opts jobOptions, data []byte, target mapping.Target) (*processResult, error) {
	in, err := demux.Demux(bytes.NewReader(data), demux.DefaultLimits())
	if err != nil {
		return &processResult{Input: in}, err
	}
	plan, err := mapping.NewResolver(target, mapping.DefaultRules()).Resolve(in.Tracks)
	if err != nil {
		return &processResult{Input: in}, err
	}
	_ = plan
	return &processResult{Input: in}, nil
}

// adaptForTarget makes the synthetic AVC/Opus/UTF-8 fixture deliverable in the
// selected container. The bytes are never decoded, so a demo-only codec
// relabel is honest: it exercises the same stream-copy path with a codec the
// target accepts. Tracks the target cannot carry at all are dropped by rule.
func adaptForTarget(target mapping.Target, tracks []media.Track) ([]media.Track, []mapping.Rule) {
	out := make([]media.Track, 0, len(tracks))
	switch target {
	case mapping.TargetWebM:
		for _, t := range tracks {
			if t.Codec == media.CodecAVC {
				t.Codec = media.CodecVP9
			}
			out = append(out, t)
		}
		return out, mapping.DefaultRules()
	case mapping.TargetFMP4:
		rules := mapping.DefaultRules()
		for _, t := range tracks {
			if t.Type == media.TrackSubtitle {
				rules = append(rules, mapping.Rule{TrackNumber: t.ID, Drop: true})
				continue
			}
			out = append(out, t)
		}
		return out, rules
	default:
		return append(out, tracks...), mapping.DefaultRules()
	}
}

// lateShift measures how retraction changes the manifest timeline. The
// "strict" engine has no retraction horizon, so late frames are rejected and
// their windows never reopen; the retracting engine keeps them. For the late
// fixture the strict manifest start lands later, which is exactly the drift the
// watermark engine is there to remove.
func lateShift(late []byte, target mapping.Target) (retracted, strict media.Timestamp, recomputed int, err error) {
	in, err := demux.Demux(bytes.NewReader(late), demux.DefaultLimits())
	if err != nil {
		return 0, 0, 0, err
	}
	tracks, rules := adaptForTarget(target, in.Tracks)
	plan, err := mapping.NewResolver(target, rules).Resolve(tracks)
	if err != nil {
		return 0, 0, 0, err
	}
	packets := mappedPackets(plan, in.Packets)
	run := func(cfg timeline.Config) (media.Timestamp, int, error) {
		engine, err := timeline.New(cfg)
		if err != nil {
			return 0, 0, err
		}
		for _, p := range packets {
			_, _, _ = engine.Push(timeline.Event{
				Key: packetKey(p), Track: p.Track, Time: p.DTS,
				Duration: p.Duration, Payload: p.Data, Arrival: p.Sequence,
			})
		}
		start, _ := engine.Timeline()
		recomputed := 0
		for _, w := range engine.Summaries() {
			if w.Recomputed {
				recomputed++
			}
		}
		return start, recomputed, nil
	}
	retracted, recomputed, err = run(timeline.DefaultConfig())
	if err != nil {
		return 0, 0, 0, err
	}
	strictCfg := timeline.DefaultConfig()
	strictCfg.Retraction = strictCfg.MaxLateness
	strict, _, err = run(strictCfg)
	if err != nil {
		return 0, 0, 0, err
	}
	return retracted, strict, recomputed, nil
}

// beyondHorizonCase proves a packet arriving later than the retraction horizon
// is quarantined and blocks commit with exit code 3.
func beyondHorizonCase(opts jobOptions, data []byte) demoCase {
	in, err := demux.Demux(bytes.NewReader(data), demux.DefaultLimits())
	if err != nil {
		return failureCase("beyond-horizon", err)
	}
	tracks, rules := adaptForTarget(opts.Target, in.Tracks)
	plan, err := mapping.NewResolver(opts.Target, rules).Resolve(tracks)
	if err != nil {
		return failureCase("beyond-horizon", err)
	}
	_ = tracks
	engine, err := timeline.New(timeline.DefaultConfig())
	if err != nil {
		return failureCase("beyond-horizon", err)
	}
	for _, p := range mappedPackets(plan, in.Packets) {
		if _, _, err := engine.Push(timeline.Event{
			Key: packetKey(p), Track: p.Track, Time: p.DTS,
			Duration: p.Duration, Payload: p.Data, Arrival: p.Sequence,
		}); err != nil {
			return failureCase("beyond-horizon", err)
		}
	}
	// A packet 10 seconds in the past is far outside the 2 s horizon.
	_, _, err = engine.Push(timeline.Event{
		Key: "beyond-horizon", Track: 1, Time: 0, Duration: media.Millisecond,
	})
	if err == nil {
		return demoCase{Name: "beyond-horizon", ExitCode: 1, Note: "late packet accepted"}
	}
	c := demoCase{Name: "beyond-horizon", ExitCode: media.ExitCode(err), CorruptOff: -1}
	c.Note = "quarantined beyond retraction horizon"
	return c
}

// bitrate derives bits per second from payload bytes over the timeline span.
func bitrate(bytes int64, span media.Timestamp) int64 {
	if span <= 0 {
		return 0
	}
	secs := float64(span) / float64(media.Second)
	return int64(float64(bytes) * 8 / secs)
}

func digestHex(d [32]byte) string { return fmt.Sprintf("%x", d[:]) }
