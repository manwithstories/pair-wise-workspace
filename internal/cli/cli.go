// Package cli implements the archivecheck command line.
//
// Exit codes: 0 success (verified, or recovered by rule), 2 corrupt input that
// cannot be recovered, 3 runtime or output failure.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"archivecheck.local/archivecheck/internal/fault"
	"archivecheck.local/archivecheck/internal/fixture"
	"archivecheck.local/archivecheck/internal/fsguard"
	"archivecheck.local/archivecheck/internal/pipeline"
)

// ExitError carries an explicit exit code without printing a stage line.
type ExitError struct {
	Code    int
	Message string
}

func (e *ExitError) Error() string { return e.Message }

const usage = `archivecheck - Matroska/WebM archive verification

usage:
  archivecheck demo    --seed N [--queue N] [--slow-consumer-ms N] [--keep DIR]
  archivecheck verify  --fixture KIND --recover=false [--keep DIR]
  archivecheck recover --fixture KIND --recover=true  [--keep DIR]
  archivecheck inspect --input FILE [--root DIR] [--recover=BOOL]

fixtures: ` + "clean, truncated, oversized, badblock, unknown-size, no-cues, out-of-order, slow-consumer"

// Run dispatches one command. out receives results; errOut receives errors.
func Run(args []string, out, errOut io.Writer) error {
	if len(args) == 0 {
		fmt.Fprintln(errOut, usage)
		return &ExitError{Code: 3, Message: "missing subcommand"}
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], out, errOut)
	case "verify":
		return runVerify(args[1:], out, errOut, false)
	case "recover":
		return runVerify(args[1:], out, errOut, true)
	case "inspect":
		return runInspect(args[1:], out, errOut)
	case "-h", "--help", "help":
		fmt.Fprintln(out, usage)
		return nil
	default:
		fmt.Fprintln(errOut, usage)
		return &ExitError{Code: 3, Message: fmt.Sprintf("unknown subcommand %q", args[0])}
	}
}

// demoOptions mirror the acceptance command.
type demoOptions struct {
	seed           int64
	queue          int
	slowConsumerMS int
	keep           string
	rtp            bool
}

func runDemo(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var o demoOptions
	fs.Int64Var(&o.seed, "seed", 17, "fixture seed")
	fs.IntVar(&o.queue, "queue", 4, "bounded queue capacity")
	fs.IntVar(&o.slowConsumerMS, "slow-consumer-ms", 2, "per-block consumer delay")
	fs.StringVar(&o.keep, "keep", "", "directory that keeps generated fixtures and output")
	fs.BoolVar(&o.rtp, "rtp", false, "also validate RTP packet structure with pion/rtp")
	if err := fs.Parse(args); err != nil {
		return &ExitError{Code: 3, Message: err.Error()}
	}
	if o.queue <= 0 {
		return &ExitError{Code: 3, Message: "--queue must be positive"}
	}

	rootDir := o.keep
	if rootDir == "" {
		dir, err := os.MkdirTemp("", "archivecheck-demo-*")
		if err != nil {
			return &ExitError{Code: 3, Message: err.Error()}
		}
		defer os.RemoveAll(dir)
		rootDir = dir
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return &ExitError{Code: 3, Message: err.Error()}
	}
	root, err := fsguard.OpenRoot(rootDir)
	if err != nil {
		return &ExitError{Code: 3, Message: err.Error()}
	}

	// Two clean documents: the second run proves the mapping digest is stable.
	var reports []*pipeline.Report
	for i := 0; i < 2; i++ {
		fx, err := fixture.New(fixture.DamageClean, o.seed)
		if err != nil {
			return &ExitError{Code: 3, Message: err.Error()}
		}
		in := fmt.Sprintf("demo-%d.mkv", i)
		if err := os.WriteFile(filepath.Join(root.Dir, in), fx.Bytes, 0o644); err != nil {
			return &ExitError{Code: 3, Message: err.Error()}
		}
		rep, err := pipeline.Run(context.Background(), pipeline.Options{
			InputPath:      in,
			OutputPath:     fmt.Sprintf("demo-%d.mp4", i),
			QueueCapacity:  o.queue,
			SlowConsumerMS: o.slowConsumerMS,
			Root:           root,
			RTPCheck:       o.rtp,
		})
		if err != nil {
			printStage(errOut, err)
			return &ExitError{Code: fault.ExitCode(err)}
		}
		reports = append(reports, rep)
	}
	first := reports[0]

	fmt.Fprintf(out, "doctype=%s tracks=%d unknown_tracks=%d\n", first.Info.DocType, len(first.Tracks), len(first.Unknown))
	for _, t := range first.Tracks {
		fmt.Fprintf(out, "track=%d type=%s codec=%s entry=%s blocks=%d frames=%d ticks=%d..%d keyframes=%d\n",
			t.Number, t.Type, t.CodecID, t.MP4Entry, t.Blocks, t.Frames, t.FirstTick, t.LastTick, t.Keyframes)
	}
	queryHits, queryTotal := keyframeQueries(first)
	rates := first.Rates
	if queryTotal > 0 {
		rates.KeyframeHit = 100 * float64(queryHits) / float64(queryTotal)
	}
	fmt.Fprintf(out, "roundtrip_match=%.2f%% keyframe_hit=%.2f%% segment_hash_match=%.2f%% peak_queue=%d\n",
		rates.RoundtripMatch, rates.KeyframeHit, rates.SegmentHashMatch, first.Queue.Peak)
	fmt.Fprintf(out, "queue_capacity=%d blocked_sends=%d output_size=%d output_sha256=%s\n",
		first.Queue.Capacity, first.Queue.Blocked, first.OutputSize, short(first.OutputSHA256))
	fmt.Fprintf(out, "mapping_digest=%s stable=%t\n",
		first.MappingDigest, first.MappingDigest == reports[1].MappingDigest)
	if o.rtp {
		fmt.Fprintf(out, "rtp_packets=%d valid=true\n", first.RTPPackets)
	}

	if rates.RoundtripMatch != 100 || rates.KeyframeHit != 100 || rates.SegmentHashMatch != 100 {
		return &ExitError{Code: 3, Message: "demo did not reach 100% fidelity"}
	}
	if first.Queue.Peak > o.queue {
		return &ExitError{Code: 3, Message: "peak queue exceeded capacity"}
	}
	if first.MappingDigest != reports[1].MappingDigest {
		return &ExitError{Code: 3, Message: "mapping digest is not deterministic"}
	}
	return nil
}

// keyframeQueries counts lookups that resolved to the expected output sample.
func keyframeQueries(rep *pipeline.Report) (hits, total int) {
	for _, e := range rep.Index.Entries {
		total++
		got, ok := rep.Index.AtOrBefore(e.Track, e.Tick)
		if ok && got.OutputSample == e.OutputSample {
			hits++
		}
	}
	return hits, total
}

// verifyOptions govern both verify and recover.
type verifyOptions struct {
	fixture string
	recover bool
	keep    string
	queue   int
}

func runVerify(args []string, out, errOut io.Writer, recoverDefault bool) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var o verifyOptions
	fs.StringVar(&o.fixture, "fixture", "", "fixture kind to generate")
	fs.BoolVar(&o.recover, "recover", recoverDefault, "skip provably bounded damage")
	fs.StringVar(&o.keep, "keep", "", "directory that keeps the generated fixture")
	fs.IntVar(&o.queue, "queue", 4, "bounded queue capacity")
	if err := fs.Parse(args); err != nil {
		return &ExitError{Code: 3, Message: err.Error()}
	}
	if o.fixture == "" {
		return &ExitError{Code: 3, Message: "--fixture is required"}
	}

	rootDir := o.keep
	if rootDir == "" {
		dir, err := os.MkdirTemp("", "archivecheck-verify-*")
		if err != nil {
			return &ExitError{Code: 3, Message: err.Error()}
		}
		defer os.RemoveAll(dir)
		rootDir = dir
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return &ExitError{Code: 3, Message: err.Error()}
	}
	root, err := fsguard.OpenRoot(rootDir)
	if err != nil {
		return &ExitError{Code: 3, Message: err.Error()}
	}
	fx, err := fixture.New(o.fixture, 17)
	if err != nil {
		return &ExitError{Code: 3, Message: err.Error()}
	}
	in := o.fixture + ".mkv"
	if err := os.WriteFile(filepath.Join(root.Dir, in), fx.Bytes, 0o644); err != nil {
		return &ExitError{Code: 3, Message: err.Error()}
	}

	outName := o.fixture + ".mp4"
	rep, runErr := pipeline.Run(context.Background(), pipeline.Options{
		InputPath:           in,
		OutputPath:          outName,
		Recovery:            o.recover,
		QueueCapacity:       o.queue,
		Root:                root,
		RefuseUnknownCodecs: true,
	})
	if runErr != nil {
		var re *fault.RecoveryError
		switch {
		case errors.As(runErr, &re) && o.recover:
			// Recovery stopped at damage that could still be proven; the
			// manifest records exactly what was skipped.
			printRecovered(out, re, fx.Note)
			return nil
		case errors.As(runErr, &re):
			// Recovery was possible but not requested: report as corruption.
			printStage(errOut, re.Cause)
			return &ExitError{Code: 2}
		default:
			printStage(errOut, runErr)
			code := fault.ExitCode(runErr)
			if code == 0 {
				code = 3
			}
			return &ExitError{Code: code}
		}
	}
	if o.recover && len(rep.Skipped) > 0 {
		re := &fault.RecoveryError{Skips: rep.Skipped}
		printRecoveredReport(out, errOut, re, rep, fx.Note)
		return nil
	}
	writeManifestFile(&repManifest{rep: rep}, rep.OutputPath+".manifest.json", out, errOut)
	fmt.Fprintf(out, "fixture=%s status=ok tracks=%d bytes=%d\n", o.fixture, len(rep.Tracks), rep.OutputSize)
	fmt.Fprintf(out, "roundtrip_match=%.2f%% keyframe_hit=%.2f%% segment_hash_match=%.2f%%\n",
		rep.Rates.RoundtripMatch, rep.Rates.KeyframeHit, rep.Rates.SegmentHashMatch)
	return nil
}

// printRecoveredReport prints the recovery manifest for a run that completed.
func printRecoveredReport(out, errOut io.Writer, re *fault.RecoveryError, rep *pipeline.Report, note string) {
	verified := int64(0)
	if rep.Verification != nil {
		for _, s := range rep.Verification.SourceSegments {
			if s.SHA256 != "" {
				verified++
			}
		}
	}
	fmt.Fprintf(out, "status=recovered skipped_segments=%d source_offset=%d hash_verified_segments=%d exit=0\n",
		len(re.Skips), firstOffset(re), verified)
	for _, s := range re.Skips {
		fmt.Fprintf(out, "skipped start=%d length=%d reason=%q\n", s.Start, s.Length, s.Reason)
	}
	writeManifestFile(&repManifest{rep: rep}, rep.OutputPath+".manifest.json", out, errOut)
	fmt.Fprintf(out, "skipped_bytes=%d output=%s output_sha256=%s\n",
		re.SkippedBytes(), rep.OutputPath, short(rep.OutputSHA256))
	fmt.Fprintf(out, "note=%q\n", note)
}

// printRecovered prints the manifest for a run that stopped after recovery,
// so no output file exists and no segment can be hash-verified.
func printRecovered(out io.Writer, re *fault.RecoveryError, note string) {
	fmt.Fprintf(out, "status=recovered skipped_segments=%d source_offset=%d hash_verified_segments=0 exit=0\n",
		len(re.Skips), firstOffset(re))
	for _, s := range re.Skips {
		fmt.Fprintf(out, "skipped start=%d length=%d reason=%q\n", s.Start, s.Length, s.Reason)
	}
	fmt.Fprintf(out, "skipped_bytes=%d note=%q\n", re.SkippedBytes(), note)
}

func firstOffset(re *fault.RecoveryError) int64 {
	var min int64 = -1
	for _, s := range re.Skips {
		if min < 0 || s.Start < min {
			min = s.Start
		}
	}
	if min < 0 {
		return fault.OffsetOf(re.Cause)
	}
	return min
}

// inspectOptions parse an arbitrary file.
type inspectOptions struct {
	input       string
	root        string
	recoverFlag bool
}

func runInspect(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var o inspectOptions
	fs.StringVar(&o.input, "input", "", "Matroska/WebM file")
	fs.StringVar(&o.root, "root", "", "input root directory (defaults to the file's directory)")
	fs.BoolVar(&o.recoverFlag, "recover", false, "skip provably bounded damage")
	if err := fs.Parse(args); err != nil {
		return &ExitError{Code: 3, Message: err.Error()}
	}
	if o.input == "" {
		return &ExitError{Code: 3, Message: "--input is required"}
	}
	rootDir := o.root
	if rootDir == "" {
		rootDir = filepath.Dir(o.input)
	}
	root, err := fsguard.OpenRoot(rootDir)
	if err != nil {
		return &ExitError{Code: 3, Message: err.Error()}
	}
	name := o.input
	if filepath.IsAbs(name) {
		rel, err := filepath.Rel(root.Dir, name)
		if err != nil {
			return &ExitError{Code: 3, Message: err.Error()}
		}
		name = rel
	}
	res, err := pipeline.Scan(context.Background(), root, name, o.recoverFlag)
	if err != nil {
		printStage(errOut, err)
		return &ExitError{Code: fault.ExitCode(err)}
	}
	fmt.Fprintf(out, "doctype=%s unknown_tracks=%d skipped_bytes=%d\n",
		res.Info.DocType, len(res.Unknown), res.SkippedBytes)
	for _, t := range res.Tracks {
		fmt.Fprintf(out, "track=%d type=%s codec=%s entry=%s blocks=%d frames=%d\n",
			t.Number, t.Type, t.CodecID, t.MP4Entry, t.Blocks, t.Frames)
	}
	if len(res.Unknown) > 0 {
		fmt.Fprintf(out, "unknown_codecs=%s\n", strings.Join(res.Unknown, ","))
	}
	return nil
}

// printStage renders the machine-readable stage line required by acceptance.
func printStage(w io.Writer, err error) {
	var fe *fault.Error
	if errors.As(err, &fe) {
		fmt.Fprintf(w, "status=error stage=%s offset=%d exit=%d\n", fe.Stage, offsetFor(fe), fault.ExitCode(err))
		if fe.Track != 0 {
			fmt.Fprintf(w, "track=%d detail=%v\n", fe.Track, err)
			return
		}
		fmt.Fprintf(w, "detail=%v\n", err)
		return
	}
	fmt.Fprintf(w, "status=error stage=runtime offset=-1 exit=3\n")
	fmt.Fprintf(w, "detail=%v\n", err)
}

func offsetFor(fe *fault.Error) int64 {
	if fe.Offset < 0 {
		return 0
	}
	return fe.Offset
}

func short(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}
