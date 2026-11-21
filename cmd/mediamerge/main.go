// Command mediamerge is the terminal acceptance adapter for the media
// pipeline. It generates fixtures, runs the staged delivery and recovery
// demos, and prints machine-readable metric lines.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mediamerge/internal/audio"
	"mediamerge/internal/fault"
	"mediamerge/internal/pipeline"
)

const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `mediamerge - deterministic media delivery pipeline

usage:
  mediamerge demo   [flags]   generate fixtures, run stages, print metrics
  mediamerge verify [flags]   re-check an existing work directory
  mediamerge help

demo flags:
  --seed int                 fixture variation seed (default 42)
  --chunk-bytes int          streaming block size (default 65536)
  --consumer-delay-ms int    simulated slow-consumer latency per block (default 20)
  --fail-after-stage int     abort after N committed stages, then recover (default 0)
  --resume                   resume from the existing journal
  --work-dir string          working directory (default ./.mediamerge)
  --corrupt string           emit one damaged fixture and exit non-zero
                             (truncated-box|truncated-partition|non-monotonic|bad-crop)
  --memory-limit-bytes int   streaming payload budget (default 1048576)
  --target-rate int          audio delivery sample rate (default 48000)
  --target-layout string     audio delivery layout (default stereo)

exit codes: 0 verified, 1 processing/validation failure, 2 usage error
`)
}

type demoFlags struct {
	seed          int64
	chunkBytes    int
	consumerDelay int
	failAfter     int
	resume        bool
	workDir       string
	corrupt       string
	memoryLimit   int64
	targetRate    int
	targetLayout  string
}

func parseDemoFlags(args []string) (demoFlags, error) {
	var f demoFlags
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Int64Var(&f.seed, "seed", 42, "fixture variation seed")
	fs.IntVar(&f.chunkBytes, "chunk-bytes", 65536, "streaming block size")
	fs.IntVar(&f.consumerDelay, "consumer-delay-ms", 20, "simulated slow-consumer latency per block")
	fs.IntVar(&f.failAfter, "fail-after-stage", 0, "abort after N committed stages")
	fs.BoolVar(&f.resume, "resume", false, "resume from the existing journal")
	fs.StringVar(&f.workDir, "work-dir", filepath.Join(".", ".mediamerge"), "working directory")
	fs.StringVar(&f.corrupt, "corrupt", "", "emit one damaged fixture and exit non-zero")
	fs.Int64Var(&f.memoryLimit, "memory-limit-bytes", 1<<20, "streaming payload budget")
	fs.IntVar(&f.targetRate, "target-rate", 48000, "audio delivery sample rate")
	fs.StringVar(&f.targetLayout, "target-layout", "stereo", "audio delivery layout")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if fs.NArg() > 0 {
		return f, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return f, nil
}

func runDemo(args []string, stdout, stderr *os.File) int {
	f, err := parseDemoFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintf(stderr, "argument error: %v\n", err)
		return exitUsage
	}
	if f.corrupt != "" {
		return runCorrupt(f, stdout, stderr)
	}
	opts := pipeline.DefaultOptions()
	opts.Seed = f.seed
	opts.ChunkBytes = f.chunkBytes
	opts.ConsumerDelayMs = f.consumerDelay
	opts.WorkDir = f.workDir
	opts.MemoryLimitBytes = f.memoryLimit
	opts.TargetSampleRate = f.targetRate
	opts.TargetLayout = audio.LayoutName(f.targetLayout)

	// A crash simulation always includes the recovery pass: the acceptance
	// run asks for both in one process, and printing only the crash would
	// leave the operator without the recovered numbers.
	if f.failAfter > 0 {
		if err := os.RemoveAll(f.workDir); err != nil {
			fmt.Fprintf(stderr, "cannot reset work directory: %v\n", err)
			return exitFail
		}
		crashOpts := opts
		crashOpts.FailAfterStage = f.failAfter
		crashOpts.Resume = false
		first, err := pipeline.Run(context.Background(), crashOpts)
		if first == nil {
			first = &pipeline.Result{}
		}
		if err != nil && !pipeline.IsInterrupted(err) {
			printResult(stdout, first, crashOpts)
			printFault(stderr, err)
			return exitFail
		}
		fmt.Fprintf(stdout, "INTERRUPTED stage=%s committed=%d\n", first.FailStage, len(first.CommittedStages))
		opts.Resume = true
		opts.FailAfterStage = 0
		resumeExpectedROLLEDBACK = true
	} else if !f.resume {
		if err := os.RemoveAll(f.workDir); err != nil {
			fmt.Fprintf(stderr, "cannot reset work directory: %v\n", err)
			return exitFail
		}
	} else {
		opts.Resume = true
	}

	res, runErr := pipeline.Run(context.Background(), opts)
	if res == nil {
		res = &pipeline.Result{}
	}
	printResult(stdout, res, opts)
	if runErr != nil {
		printFault(stderr, runErr)
		return exitFail
	}
	if err := checkAcceptance(res); err != nil {
		printFault(stderr, err)
		return exitFail
	}
	return exitOK
}

func printResult(w *os.File, res *pipeline.Result, opts pipeline.Options) {
	fmt.Fprintf(w, "PARSE_ROUNDTRIP_RATE %.3f (%d/%d)\n",
		res.Parse.RoundtripRate(), res.Parse.TracksMatched, res.Parse.TracksCompared)
	fmt.Fprintf(w, "KEYFRAME_HIT_RATE %.3f (%d/%d)\n",
		res.Parse.KeyframeRate(), res.Parse.KeyHits, res.Parse.KeyProbes)
	fmt.Fprintf(w, "SEGMENT_HASH_MATCH %d/%d\n", res.SegmentHashes, res.SegmentHashesTotal)
	fmt.Fprintf(w, "WAL_RECOVERED %d\n", boolInt(res.WALRecovered))
	fmt.Fprintf(w, "COMMITTED_STAGES %d %s\n", len(res.CommittedStages), strings.Join(res.CommittedStages, ","))
	fmt.Fprintf(w, "ROLLED_BACK_BYTES %d\n", res.RolledBackBytes)
	fmt.Fprintf(w, "WAL_TRUNCATED_BYTES %d\n", res.WALTruncatedBytes)
	fmt.Fprintf(w, "QUEUE_HIGH_WATER %d\n", res.Queue.HighWater)
	fmt.Fprintf(w, "QUEUE_BLOCKED %d\n", res.Queue.Blocked)
	fmt.Fprintf(w, "CONSUMER_BLOCKS %d\n", res.ConsumerBlocks)
	fmt.Fprintf(w, "MEMORY_HIGH_WATER %d\n", res.MemoryHighWater)
	for _, name := range []string{"parse", "audio", "mux", "final"} {
		if d, ok := res.StageDigests[name]; ok {
			fmt.Fprintf(w, "STAGE_DIGEST %s %s\n", name, d)
		}
	}
	for _, b := range res.SegmentDigests {
		fmt.Fprintf(w, "SEGMENT %d track=%d offset=%d bytes=%d digest=%x\n",
			b.Segment, b.TrackID, b.Offset, b.Bytes, b.Digest[:8])
	}
	for _, a := range res.Audio {
		fmt.Fprintf(w, "AUDIO_SPEC track=%d in_rate=%d out_rate=%d in_layout=%s out_layout=%s frames=%d gain_db=%.2f limited=%d\n",
			a.TrackID, a.InRate, a.OutRate, a.InLayout, a.OutLayout, a.Frames, a.GainDB, boolInt(a.Limited))
	}
	for _, l := range res.Loudness {
		reason := l.LimitReason
		if reason == "" {
			reason = "NONE"
		}
		fmt.Fprintf(w, "LOUDNESS in_lufs=%.2f applied_gain_db=%.2f target_lufs=%.2f input_peak=%.3f limited=%d reason=%s\n",
			l.InputLUFS, l.AppliedGainDB, l.TargetLUFS, l.InputPeak, boolInt(l.Limited), reason)
	}
	fmt.Fprintf(w, "OUTPUT %s\n", res.OutputPath)
}

func printFault(w *os.File, err error) {
	var fe *fault.Error
	if errors.As(err, &fe) {
		fmt.Fprintf(w, "FAULT stage=%s code=%s track=%d detail=%q\n",
			fe.Stage, fe.Code, fe.TrackID, fe.Detail)
		fmt.Fprintf(w, "fault class=%s: %v\n", fault.Classify(err), err)
		return
	}
	fmt.Fprintf(w, "FAULT %v\n", err)
}

// resumeExpectedROLLEDBACK records that this invocation performed a crash
// simulation, which requires a non-zero rollback on the recovery pass.
var resumeExpectedROLLEDBACK bool

// checkAcceptance enforces the thresholds from the delivery contract.
func checkAcceptance(res *pipeline.Result) error {
	var problems []string
	if res.Parse.TracksCompared == 0 || res.Parse.RoundtripRate() != 1 {
		problems = append(problems, fmt.Sprintf("parse roundtrip rate %.3f", res.Parse.RoundtripRate()))
	}
	if res.Parse.KeyProbes == 0 || res.Parse.KeyframeRate() != 1 {
		problems = append(problems, fmt.Sprintf("keyframe hit rate %.3f", res.Parse.KeyframeRate()))
	}
	if res.SegmentHashes < 8 || res.SegmentHashes != res.SegmentHashesTotal {
		problems = append(problems, fmt.Sprintf("segment hash match %d/%d", res.SegmentHashes, res.SegmentHashesTotal))
	}
	if len(res.CommittedStages) < 2 {
		problems = append(problems, fmt.Sprintf("committed stages %d", len(res.CommittedStages)))
	}
	if resumeExpectedROLLEDBACK {
		if !res.WALRecovered {
			problems = append(problems, "recovery pass did not observe WAL state")
		}
		if res.RolledBackBytes <= 0 {
			problems = append(problems, fmt.Sprintf("rolled-back bytes %d", res.RolledBackBytes))
		}
		if len(res.CommittedStages) < 2 {
			problems = append(problems, "recovery pass committed fewer than 2 stages")
		}
	}
	if len(problems) > 0 {
		return fault.New(fault.StageValidate, fault.CodeChecksumMismatch, strings.Join(problems, "; "))
	}
	return nil
}

func runCorrupt(f demoFlags, stdout, stderr *os.File) int {
	dir := filepath.Join(f.workDir, "corrupt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(stderr, "cannot create corrupt fixture directory: %v\n", err)
		return exitFail
	}
	path, err := pipeline.WriteCorruptFixture(dir, f.corrupt)
	if err != nil {
		printFault(stderr, err)
		if fault.Classify(err) == fault.ClassFatal {
			var fe *fault.Error
			if errors.As(err, &fe) && fe.Code == fault.CodeInvalidArgument {
				return exitUsage
			}
		}
		return exitFail
	}
	fmt.Fprintf(stdout, "CORRUPT_FIXTURE %s %s\n", f.corrupt, path)
	_, err = pipeline.Inspect(path)
	if err != nil {
		printFault(stdout, err)
		fmt.Fprintf(stdout, "CORRUPT_LOCATED %d\n", boolInt(true))
		return exitFail
	}
	fmt.Fprintln(stderr, "corrupt fixture unexpectedly parsed cleanly")
	return exitFail
}

func runVerify(args []string, stdout, stderr *os.File) int {
	f, err := parseDemoFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "argument error: %v\n", err)
		return exitUsage
	}
	res, err := pipeline.Verify(f.workDir)
	if err != nil {
		printFault(stderr, err)
		return exitFail
	}
	printResult(stdout, res, pipeline.DefaultOptions())
	if err := checkAcceptance(res); err != nil {
		printFault(stderr, err)
		return exitFail
	}
	return exitOK
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
