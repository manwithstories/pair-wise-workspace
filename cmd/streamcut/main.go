// Command streamcut is the terminal entry point for the regional live
// distribution pipeline. It generates a deterministic MPEG-TS fixture, runs
// the bounded cutting/silence/dedup/packaging graph and prints one metrics
// report. Exit codes follow the operations contract:
//
//	0 success or recovered, 2 unrecoverable input corruption,
//	3 resource limit or cancellation, 1 internal error.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"streamcut/internal/audio"
	"streamcut/internal/fixture"
	"streamcut/internal/media"
	"streamcut/internal/metrics"
	"streamcut/internal/pipeline"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "demo":
		os.Exit(runDemo(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `streamcut — MPEG-TS live segment cutter

usage:
  streamcut demo --case <clean|damaged|resume> --seed <n> --target-bps <bps>
                 --queue-depth <n> --slow-consumer-ms <n> --mem-limit-mib <n>
`)
}

type demoFlags struct {
	casename       string
	seed           int64
	targetBPS      int64
	queueDepth     int
	slowConsumerMS int64
	memLimitMiB    int64
}

func parseDemo(args []string) (demoFlags, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	var d demoFlags
	fs.StringVar(&d.casename, "case", "clean", "fixture scenario")
	fs.Int64Var(&d.seed, "seed", 17, "deterministic generator seed")
	fs.Int64Var(&d.targetBPS, "target-bps", 4_000_000, "target bitrate bps")
	fs.IntVar(&d.queueDepth, "queue-depth", 2, "bounded queue depth")
	fs.Int64Var(&d.slowConsumerMS, "slow-consumer-ms", 25, "per-segment consumer delay")
	fs.Int64Var(&d.memLimitMiB, "mem-limit-mib", 8, "hard memory ceiling in MiB")
	if err := fs.Parse(args); err != nil {
		return d, err
	}
	return d, nil
}

func runDemo(args []string) int {
	d, err := parseDemo(args)
	if err != nil {
		return 1
	}
	c := fixture.Case(d.casename)
	cfg := fixture.DefaultConfig(d.seed, d.targetBPS, c)
	res, err := fixture.Generate(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixture generation failed: %v\n", err)
		return 1
	}

	opts := pipeline.Options{
		QueueDepth:      d.queueDepth,
		MemLimitBytes:   d.memLimitMiB * 1024 * 1024,
		SlowConsumerMS:  d.slowConsumerMS,
		TargetSegmentMS: 2000,
	}
	durationTicks := media.TicksPerSecond * media.Time90(cfg.DurationS)
	totalBytes := int64(res.Bytes)
	silenceRef := toAudioIntervals(res.Silence)

	report := metrics.New(d.targetBPS)

	switch c {
	case fixture.CaseResume:
		exit := runResume(cfg, res, opts, durationTicks, totalBytes, silenceRef, report)
		emit(report)
		return exit
	default:
		out, perr := pipeline.Run(context.Background(), opts, res.Packets,
			silenceRef, durationTicks, totalBytes, nil)
		if perr != nil {
			return mapError(perr)
		}
		fillReport(report, out, totalBytes, durationTicks)
		emit(report)
		if unrecoverable(out) {
			return 2
		}
		return 0
	}
}

// runResume simulates an interrupted-then-restarted distribution job. The
// stream is split at a fixed packet boundary; the head runs to a checkpoint,
// then the tail resumes with that checkpoint. The published-id hash must equal
// a single uninterrupted run, proving no segment is published twice.
func runResume(cfg fixture.Config, res *fixture.Result, opts pipeline.Options,
	durationTicks media.Time90, totalBytes int64, ref []audio.Interval,
	report *metrics.Report) int {
	totalPackets := len(res.Packets) + len(res.Tail)
	split := totalPackets * 2 / 3
	cfg.ResumeSplit = split
	splitRes, err := fixture.Generate(cfg)
	if err != nil {
		return 1
	}

	// Reference: one uninterrupted run over the complete stream.
	full := append(append([][]byte(nil), splitRes.Packets...), splitRes.Tail...)
	refOut, err := pipeline.Run(context.Background(), opts, full, ref,
		durationTicks, totalBytes, nil)
	if err != nil {
		return mapError(err)
	}

	// Interrupted run: the head ends on an IDR-aligned segment boundary and
	// checkpoints every id already published. The tail starts a fresh cutter
	// at the next IDR; segment ids are content-addressed, so the tail's
	// segments hash identically to the uninterrupted run's and the checkpoint
	// suppresses any id the head already shipped.
	head, err := pipeline.Run(context.Background(), opts, splitRes.Packets,
		ref, durationTicks, totalBytes, nil)
	if err != nil {
		return mapError(err)
	}
	cp := pipeline.NewCheckpoint()
	for _, id := range head.PublishedIDs {
		cp.Published[id] = struct{}{}
	}
	// Seed the tail's timeline so its raw PTS values extend onto the same
	// absolute axis as the uninterrupted run, keeping every cut boundary
	// identical and every segment id content-stable.
	tailOpts := opts
	tailOpts.TimelineOrigin = splitRes.SplitExtendedPTS
	tailOpts.OriginRaw = splitRes.SplitRawPTS
	tail, err := pipeline.Run(context.Background(), tailOpts, splitRes.Tail,
		ref, durationTicks, totalBytes, cp)
	if err != nil {
		return mapError(err)
	}
	merged := append(append([]string(nil), head.PublishedIDs...), tail.PublishedIDs...)
	resumeHash := pipeline.HashIDs(merged)

	fillReport(report, refOut, totalBytes, durationTicks)
	report.ResumeHash = resumeHash
	if resumeHash != refOut.ResumeHash {
		report.Note("resume hash mismatch with uninterrupted run")
		return 1
	}
	report.Note(fmt.Sprintf("resume matched uninterrupted hash=%s", resumeHash))
	return 0
}

func fillReport(r *metrics.Report, out *pipeline.Result, totalBytes int64,
	durationTicks media.Time90) {
	r.SetActual(totalBytes, int64(durationTicks), int64(media.TicksPerSecond))
	r.Segments = out.Report.Segments
	r.CCErrors = out.Report.CCErrors
	r.PSICRCRejects = out.Report.PSICRCRejects
	r.SilencePrecision = out.Report.Silence.Precision
	r.SilenceRecall = out.Report.Silence.Recall
	r.SilenceF1 = out.Report.Silence.F1
	r.DuplicateHits = out.Report.DuplicateHits
	r.ManifestSkewMS = out.Report.ManifestSkewMS
	r.BufferPeakBytes = out.Report.BufferPeakBytes
	r.ResumeHash = out.ResumeHash
	r.UnsupportedCodecs = out.Report.Unsupported
	r.Truncated = out.Report.Truncated
	r.SilenceIntervals = out.Report.SilenceCount
}

func unrecoverable(out *pipeline.Result) bool {
	return out.Report.Unsupported > 0
}

func mapError(err error) int {
	switch media.CodeOf(err) {
	case media.EResourceLimit, media.ECancelled:
		return 3
	case media.EUnrecoverable:
		return 2
	default:
		return 1
	}
}

func emit(r *metrics.Report) {
	if err := metrics.WriteJSON(os.Stdout, r); err != nil {
		fmt.Fprintf(os.Stderr, "report write failed: %v\n", err)
	}
}

func toAudioIntervals(in []fixture.Interval) []audio.Interval {
	out := make([]audio.Interval, 0, len(in))
	for _, iv := range in {
		out = append(out, audio.Interval{Start: iv.Start, End: iv.End})
	}
	return out
}
