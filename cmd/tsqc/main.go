// Command tsqc is the terminal adapter for the tsqc pipeline: it generates
// deterministic sample streams, runs the RTP/TS recovery and segmentation
// pipeline, and verifies the archived products.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"tsqc/internal/clock"
	"tsqc/internal/fixture"
	"tsqc/internal/pipeline"
)

const usage = `tsqc - MPEG-TS/RTP ingest quality checker

usage:
  tsqc demo   --seed N [--faults all|list] --state DIR --out DIR
  tsqc verify --dir DIR

A demo run resumes automatically from a compatible checkpoint in --state;
an incompatible or corrupt checkpoint is discarded and recomputed.

exit codes: 0 pass, 1 unplayable stream, 2 bad arguments or digest mismatch
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var code int
	switch os.Args[1] {
	case "demo":
		code = runDemo(os.Args[2:])
	case "verify":
		code = runVerify(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s", os.Args[1], usage)
		code = 2
	}
	os.Exit(code)
}

// exitCode maps an error onto the documented contract: 2 for bad arguments
// or product/digest integrity failures, 1 when the stream itself cannot be
// made playable.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	switch {
	case clock.IsCode(err, clock.CodeDigestMismatch),
		clock.IsCode(err, clock.CodeCacheMismatch),
		clock.IsCode(err, clock.CodeCheckpoint),
		clock.IsCode(err, clock.CodeInvalidArgument),
		clock.IsRecoverable(err):
		return 2
	}
	return 1
}

func runPipeline(seed int64, faults []string, state, out string) (*pipeline.Report, error) {
	return pipeline.Run(pipeline.RunOptions{
		Seed: seed, Faults: faults, State: state, Out: out,
	})
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 41, "deterministic fixture seed")
	faultsArg := fs.String("faults", "", "comma-separated faults or all")
	stateDir := fs.String("state", "", "state directory for cache and checkpoint")
	outDir := fs.String("out", "", "output directory for segments and report")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *stateDir == "" || *outDir == "" {
		fmt.Fprintln(os.Stderr, "demo requires --state and --out")
		return 2
	}
	var names []string
	if *faultsArg != "" {
		names = strings.Split(*faultsArg, ",")
	}
	faults, err := fixture.ParseFaults(names)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	report, err := runPipeline(*seed, faults, *stateDir, *outDir)
	if err != nil {
		return exitCode(err)
	}
	printReport(report, os.Stdout)
	if report.Fatal {
		return 1
	}
	return 0
}

func printReport(r *pipeline.Report, w io.Writer) {
	status := "PASS"
	if r.Fatal {
		status = "UNPLAYABLE"
	}
	fmt.Fprintf(w, "status=%s\n", status)
	fmt.Fprintf(w, "source=%s\n", r.Source)
	fmt.Fprintf(w, "seed=%d faults=%s\n", r.Seed, strings.Join(r.Faults, ","))
	fmt.Fprintf(w, "rtp_packets=%d ts_bytes=%d\n", r.RTPPackets, r.TSBytes)
	fmt.Fprintf(w, "segments=%d\n", r.Segments)
	fmt.Fprintf(w, "gaps_recovered=%d gaps_unrecoverable=%d\n", r.Recovered, r.Unrecoverable)
	fmt.Fprintf(w, "roundtrip_consistency=%.2f%%\n", r.Roundtrip)
	fmt.Fprintf(w, "keyframe_hit_rate=%.2f%%\n", r.KeyframeHitRate)
	fmt.Fprintf(w, "cache_hits=%d cache_misses=%d checkpoint_saved=%d resumed=%d\n",
		r.CacheHits, r.CacheMisses, r.CheckpointSaved, r.ResumedSegments)
	for i, d := range r.Digests {
		fmt.Fprintf(w, "segment=%02d sha256=%s\n", i, d)
	}
	for _, g := range r.Gaps {
		fmt.Fprintf(w, "gap pid=%#04x pts=%d..%d resume=%d idr=%v reason=%s\n",
			g.PID, g.StartPTS, g.EndPTS, g.ResumePTS, g.ResumeIDR, g.Reason)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "note=%s\n", n)
	}
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory written by demo")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "verify requires --dir")
		return 2
	}
	m, err := pipeline.VerifyDir(*dir)
	if err != nil {
		return exitCode(err)
	}
	fmt.Printf("verify=OK source=%s segments=%d roundtrip_consistency=%.2f%% keyframe_hit_rate=%.2f%%\n",
		m.Source, len(m.Segments), m.Roundtrip, m.KeyframeHitRate)
	for _, s := range m.Segments {
		fmt.Printf("segment=%02d sha256=%s content_sha256=%s size=%d\n", s.Index, s.SHA256, s.ContentSHA, s.Size)
	}
	return 0
}
