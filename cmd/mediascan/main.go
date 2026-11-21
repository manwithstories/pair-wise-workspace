// Command mediascan validates regional HLS/DASH distribution end to end:
// it scans segments into a digest index, validates box structure and sample
// clocks, measures A/V skew and keyframe spacing, audits RTP loss/reordering,
// and writes reproducible provenance that a later verify can re-check.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mediascan/internal/fixture"
	"mediascan/internal/pipeline"
)

// Exit codes are part of the contract.
const (
	exitOK       = 0
	exitUsage    = 1
	exitMismatch = 2
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitUsage
	}
	switch args[0] {
	case "demo":
		return cmdDemo(args[1:])
	case "resume":
		return cmdResume(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	case "-h", "--help", "help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "mediascan: unknown command %q\n", args[0])
		usage()
		return exitUsage
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mediascan - HLS/DASH distribution validator

usage:
  mediascan demo   --work DIR [--loss-every N] [--interrupt-after N] [--rtp-packets N]
  mediascan resume --work DIR
  mediascan verify --work DIR

demo generates self-contained inputs (Manifest touching every media slot, MP4
fragments with injected faults, and an RTP capture), scans them, and writes a
checkpoint plus JSONL provenance. resume continues an interrupted run. verify
recomputes digests and exits 2 on any input or result mismatch.
`)
}

type commonFlags struct {
	work       string
	lossEvery  int
	interrupt  int
	rtpPackets int
}

func parseCommon(name string, args []string) (commonFlags, error) {
	var cf commonFlags
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&cf.work, "work", "", "work directory (required, confined to this directory)")
	fs.IntVar(&cf.lossEvery, "loss-every", 17, "drop every Nth RTP packet")
	fs.IntVar(&cf.rtpPackets, "rtp-packets", 240, "number of RTP packets to generate")
	fs.IntVar(&cf.interrupt, "interrupt-after", 0, "stop after N scanned segments (demo only)")
	if err := fs.Parse(args); err != nil {
		return cf, err
	}
	if cf.work == "" {
		return cf, errors.New("--work is required")
	}
	if cf.lossEvery <= 1 {
		return cf, errors.New("--loss-every must be greater than 1")
	}
	if cf.rtpPackets <= 0 || cf.rtpPackets > 5000 {
		return cf, errors.New("--rtp-packets must be between 1 and 5000")
	}
	if cf.interrupt < 0 {
		return cf, errors.New("--interrupt-after must be >= 0")
	}
	return cf, nil
}

func optionsFor(cf commonFlags) pipeline.Options {
	return pipeline.Options{
		Work:       cf.work,
		LossEvery:  cf.lossEvery,
		RTPPackets: cf.rtpPackets,
	}
}

func cmdDemo(args []string) int {
	cf, err := parseCommon("demo", args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediascan demo: %v\n", err)
		return exitUsage
	}
	work, err := prepareWork(cf.work)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediascan demo: %v\n", err)
		return exitUsage
	}
	layout, err := fixture.Generate(work)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediascan demo: generate: %v\n", err)
		return exitUsage
	}
	dropped, err := fixture.GenerateRTP(layout.RTPFile, cf.rtpPackets, cf.lossEvery)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediascan demo: rtp: %v\n", err)
		return exitUsage
	}
	fmt.Printf("generated: hls=%s dash=%s segments=%d rtp_packets=%d dropped=%d\n",
		rel(work, layout.HLS), rel(work, layout.DASH), fixture.SegmentCount, cf.rtpPackets, dropped)
	res, err := pipeline.Run(optionsFor(cf), cf.interrupt)
	if err != nil && !errors.Is(err, pipeline.ErrInterrupted) {
		fmt.Fprintf(os.Stderr, "mediascan demo: %v\n", err)
		return exitUsage
	}
	printResult(res)
	if errors.Is(err, pipeline.ErrInterrupted) {
		fmt.Printf("hint: run 'mediascan resume' to continue\n")
	}
	return exitOK
}

func cmdResume(args []string) int {
	cf, err := parseCommon("resume", args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediascan resume: %v\n", err)
		return exitUsage
	}
	if _, err := prepareWork(cf.work); err != nil {
		fmt.Fprintf(os.Stderr, "mediascan resume: %v\n", err)
		return exitUsage
	}
	cf.work, err = filepath.Abs(cf.work)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediascan resume: %v\n", err)
		return exitUsage
	}
	res, err := pipeline.Run(optionsFor(cf), 0)
	if err != nil && !errors.Is(err, pipeline.ErrInterrupted) {
		fmt.Fprintf(os.Stderr, "mediascan resume: %v\n", err)
		return exitUsage
	}
	printResult(res)
	fmt.Printf("resumed: status=complete\n")
	return exitOK
}

func cmdVerify(args []string) int {
	cf, err := parseCommon("verify", args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediascan verify: %v\n", err)
		return exitUsage
	}
	work, err := prepareWork(cf.work)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediascan verify: %v\n", err)
		return exitUsage
	}
	code, msg := pipeline.Verify(pipeline.Options{Work: work})
	if code != 0 {
		fmt.Fprintf(os.Stderr, "mediascan verify: %s\n", msg)
		return code
	}
	fmt.Printf("verify: ok %s\n", msg)
	return exitOK
}

// prepareWork cleans the work directory and creates it if needed. All paths
// stay under this directory.
func prepareWork(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return filepath.ToSlash(r)
	}
	return p
}

func printResult(r *pipeline.Result) {
	if r == nil {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "segments=%d scanned=%d\n", r.Segments, r.Scanned)
	fmt.Fprintf(&b, "missing=%d first_missing=%s\n", len(r.Missing), orNone(r.FirstMiss))
	fmt.Fprintf(&b, "corrupt_ranges=%d first_undecodable=%s\n", len(r.CorruptRange), orNone(firstCorrupt(r)))
	fmt.Fprintf(&b, "av_sync_max_ms=%d at=%s segment=%s\n",
		r.AVSync.MaxErrorMS, fmtRational(r.AVSync.MaxErrorAt), orNone(r.AVSync.MaxErrorSeg))
	fmt.Fprintf(&b, "av_real_jumps=%d clock_offsets=%d timestamp_wraps=%d\n",
		r.AVSync.RealJumps, r.AVSync.ClockSkews, r.AVSync.WrapSpans)
	fmt.Fprintf(&b, "keyframes irregular=%d max_gop_ms=%d\n", r.Keyframes.Irregular, r.Keyframes.MaxIntervalMS)
	fmt.Fprintf(&b, "packets_lost=%d duplicates=%d reordered=%d recovered=%d loss_recovery_pct=%.2f\n",
		r.RTP.Dropped, r.RTP.Duplicates, r.RTP.Reordered, r.RTP.Recovered, r.RTP.RecoveryPct)
	fmt.Fprintf(&b, "resume_reused_segments=%d p95_latency_ms=%.2f\n", r.ResumeReused, r.P95LatencyMS)
	fmt.Fprintf(&b, "input_sha256=%s\n", r.InputSHA)
	fmt.Fprintf(&b, "result_sha256=%s\n", r.ResultSHA)
	fmt.Fprintf(&b, "faults=%s\n", faultSummary(r))
	if r.Interrupted {
		fmt.Fprintf(&b, "status=interrupted next_segment=%d\n", r.Checkpoint)
	}
	fmt.Print(b.String())
}

func firstCorrupt(r *pipeline.Result) string {
	if len(r.CorruptRange) == 0 {
		return ""
	}
	return r.CorruptRange[0].Segment
}

func faultSummary(r *pipeline.Result) string {
	counts := map[string]int{}
	order := []string{}
	for _, f := range r.Faults {
		if counts[string(f.Kind)] == 0 {
			order = append(order, string(f.Kind))
		}
		counts[string(f.Kind)]++
	}
	if len(order) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	return strings.Join(parts, ",")
}

func fmtRational(r interface{ MillisRounded() (int64, error) }) string {
	ms, err := r.MillisRounded()
	if err != nil {
		return "?"
	}
	return fmt.Sprintf("%dms", ms)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

var _ = time.Now
