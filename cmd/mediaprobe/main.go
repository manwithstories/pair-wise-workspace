// Command mediaprobe cuts self-generated CMAF/fMP4 fragment streams into
// seekable segments, marks silence, detects duplicates and reports timeline
// and queue health. Exit codes: 0 ok, 1 unrecoverable init error,
// 2 corrupt input detected and isolated, 64 usage error.
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/pion/rtp"

	"mediaprobe/internal/audio"
	"mediaprobe/internal/clock"
	"mediaprobe/internal/fingerprint"
	"mediaprobe/internal/fixture"
	"mediaprobe/internal/index"
	"mediaprobe/internal/mp4"
	"mediaprobe/internal/report"
	"mediaprobe/internal/segment"
	"mediaprobe/internal/stream"
)

const (
	exitOK      = 0
	exitInit    = 1
	exitCorrupt = 2
	exitUsage   = 64
)

const usageText = `usage:
  mediaprobe demo   --seed N --queue N --drift-ppm PPM --wrap-at N [--fragments N] [--vfr] [--slow-consumer-ms MS]
  mediaprobe verify --seed N --corrupt MODE [--queue N]
corrupt modes: truncated-box, sample-range, sequence-gap, tfdt-regression, duplicate-segment`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usageText)
		return exitUsage
	}
	switch args[0] {
	case "demo":
		return cmdDemo(args[1:], stdout, stderr)
	case "verify":
		return cmdVerify(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n%s\n", args[0], usageText)
		return exitUsage
	}
}

func cmdDemo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Int64("seed", 42, "fixture seed")
	queue := fs.Int("queue", 8, "queue capacity")
	drift := fs.Float64("drift-ppm", 0, "injected video clock drift")
	wrap := fs.Uint64("wrap-at", 0, "capture clock modulus (0 disables wrap)")
	fragments := fs.Int("fragments", 12, "video fragments to generate")
	vfr := fs.Bool("vfr", true, "variable frame rate timestamps")
	slow := fs.Int("slow-consumer-ms", 2, "consumer delay per fragment")
	maxBytes := fs.Int("max-sample-bytes", 1<<20, "per-fragment byte cap")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	res, err := runPipeline(fixture.Options{
		Seed: *seed, Fragments: *fragments, DriftPPM: *drift, WrapAt: *wrap, VFR: *vfr,
	}, *queue, *maxBytes, time.Duration(*slow)*time.Millisecond)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return exitInit
	}
	res.summary.Print(stdout)
	return exitOK
}

func cmdVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Int64("seed", 42, "fixture seed")
	corrupt := fs.String("corrupt", "", "fault mode to inject (required)")
	queue := fs.Int("queue", 8, "queue capacity")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	mode, err := fixture.ParseCorruptMode(*corrupt)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return exitUsage
	}
	res, err := runPipeline(fixture.Options{Seed: *seed, VFR: true, Corrupt: mode}, *queue, 1<<20, 0)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return exitInit
	}
	res.summary.Print(stdout)
	detected := false
	switch mode {
	case fixture.CorruptTruncatedBox, fixture.CorruptSampleRange:
		detected = res.firstCorrupt != nil
		if detected {
			fmt.Fprintf(stdout, "bad_box_offset=%d\n", res.firstCorrupt.Offset)
		}
	case fixture.CorruptSeqGap:
		detected = res.summary.SeqGaps > 0
	case fixture.CorruptTFDTRegression:
		detected = res.summary.TFDTRegressions > 0
	case fixture.CorruptDuplicate:
		detected = res.summary.DuplicateSegments > 0
	case fixture.CorruptNone:
		detected = true
	}
	fmt.Fprintf(stdout, "recovered_segments=%d\n", res.recoveredSegments)
	if !detected {
		fmt.Fprintln(stderr, "error: injected fault was not detected")
		return exitInit
	}
	if mode == fixture.CorruptNone {
		fmt.Fprintf(stdout, "exit_code=%d\n", exitOK)
		return exitOK
	}
	fmt.Fprintf(stdout, "exit_code=%d\n", exitCorrupt)
	return exitCorrupt
}

type pipeResult struct {
	summary           report.Summary
	firstCorrupt      *mp4.CorruptError
	recoveredSegments int
	segmentsAtFault   int
	faultSeen         bool
}

// runPipeline generates a stream and pushes it through parse -> timeline ->
// cut/silence -> fingerprint/index over one bounded queue.
func runPipeline(opts fixture.Options, queueCap, maxBytes int, consumerDelay time.Duration) (*pipeResult, error) {
	gen := fixture.Generate(opts)
	init, err := mp4.ParseInit(gen.Init)
	if err != nil {
		return nil, fmt.Errorf("init segment: %w", err)
	}
	type item struct{ frag, capture []byte }
	q := stream.NewQueue[item](queueCap, maxBytes)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	res := &pipeResult{}
	var rt mp4.RoundtripResult
	tl := clock.NewTimeline()
	unw := clock.NewUnwrapper(opts.WrapAt)
	drift := &clock.DriftEstimator{}
	seqs := map[uint32]*mp4.SeqTracker{}
	cutter := segment.NewCutter(2 * clock.MediaTimescale)
	var audioBuf []segment.TimedSample

	go func() {
		defer q.Close()
		for i, f := range gen.Fragments {
			err := q.Push(ctx, item{f, gen.Capture[i]}, len(f))
			if errors.Is(err, stream.ErrSampleTooLarge) {
				res.summary.TooLargeFragments++
				continue
			}
			if err != nil {
				return // cancelled
			}
		}
	}()

	var consumeErr error
	for {
		it, ok := q.Pop()
		if !ok {
			break
		}
		frag, err := mp4.ParseFragment(it.frag, init)
		if err != nil {
			ce, ok := mp4.AsCorrupt(err)
			if !ok {
				consumeErr = err
				cancel()
				break
			}
			res.summary.CorruptFragments++
			if res.firstCorrupt == nil {
				res.firstCorrupt = ce
			}
			res.markFault(cutter)
			continue // isolate and keep processing
		}
		mp4.RoundtripFragment(frag, &rt)
		tr, _ := init.Track(frag.TrackID)
		st := seqs[frag.TrackID]
		if st == nil {
			st = mp4.NewSeqTracker()
			seqs[frag.TrackID] = st
		}
		missingBefore := st.Missing()
		st.Observe(frag.Seq)
		gapBefore := st.Missing() > missingBefore
		var durSum int64
		for _, s := range frag.Samples {
			durSum += int64(s.Duration)
		}
		mediaStart, kind := tl.Track(frag.TrackID, tr.Timescale).Align(frag.TFDT, durSum)
		if it.capture != nil {
			var pkt rtp.Packet
			if uerr := pkt.Unmarshal(it.capture); uerr == nil && len(pkt.Payload) >= 8 {
				drift.Observe(unw.Unwrap(binary.BigEndian.Uint64(pkt.Payload)), mediaStart)
			}
		}
		uncertain := kind == clock.JumpRegression
		for _, s := range frag.Samples {
			ts := segment.TimedSample{
				Sample:    s,
				MediaPTS:  mediaStart + clock.Rescale(s.DTS-frag.TFDT, tr.Timescale, clock.MediaTimescale),
				MediaDur:  clock.Rescale(int64(s.Duration), tr.Timescale, clock.MediaTimescale),
				Uncertain: uncertain,
				GapBefore: gapBefore && s.Index == 0,
			}
			if frag.TrackID == fixture.VideoTrackID {
				cutter.Add(ts)
			} else {
				audioBuf = append(audioBuf, ts)
			}
		}
		if consumerDelay > 0 {
			time.Sleep(consumerDelay)
		}
	}
	if consumeErr != nil {
		return nil, consumeErr
	}
	cutter.Flush()

	dedup := fingerprint.NewDeduper()
	ix := index.New()
	for i := range cutter.Segments() {
		seg := &cutter.Segments()[i]
		h := fingerprint.Hash(seg)
		canonical, dup := dedup.Add(seg)
		entry := report.SegmentEntry{ID: seg.ID, Start: seg.Start, End: seg.End, Hash: h, DuplicateOf: -1, Uncertain: seg.Uncertain}
		if dup {
			entry.DuplicateOf = canonical
			res.summary.DuplicateSegments++
		}
		ix.Add(seg.Start, seg.ID)
		res.summary.Segments = append(res.summary.Segments, entry)
	}
	if res.faultSeen {
		res.recoveredSegments = len(res.summary.Segments) - res.segmentsAtFault
	} else {
		res.recoveredSegments = len(res.summary.Segments)
	}

	hits := 0
	for _, e := range res.summary.Segments {
		if _, ok := ix.Exact(e.Start); ok {
			hits++
		}
	}
	res.summary.KeyframeHitPct = 100
	if len(res.summary.Segments) > 0 {
		res.summary.KeyframeHitPct = 100 * float64(hits) / float64(len(res.summary.Segments))
	}
	sil, unk := audio.NewDetector(0.001, clock.MediaTimescale/2).Analyze(audioBuf)
	res.summary.SilenceIntervals = len(sil)
	res.summary.UnknownIntervals = len(unk)

	res.summary.RoundtripPct = rt.Percent()
	res.summary.QueuePeak = q.Peak()
	res.summary.BackpressureEvents = q.BackpressureEvents()
	res.summary.DriftPPM = drift.PPM()
	for _, st := range seqs {
		res.summary.SeqGaps += st.Missing()
		res.summary.SeqDuplicates += len(st.Duplicates)
		res.summary.SeqOutOfOrder += len(st.OutOfOrder)
	}
	for _, t := range tl.Tracks() {
		res.summary.TFDTRegressions += t.Regressions
		res.summary.ForwardGaps += t.ForwardGaps
		res.summary.UncertainIntervals += len(t.Uncertain)
	}
	return res, nil
}

func (r *pipeResult) markFault(cutter *segment.Cutter) {
	if !r.faultSeen {
		r.faultSeen = true
		r.segmentsAtFault = len(cutter.Segments())
	}
}
