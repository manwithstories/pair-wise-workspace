// Command mediarun is the terminal entry point for the delivery pipeline.
//
//	exit 0: every assertion passed
//	exit 1: a verification mismatch (hash, gap or count)
//	exit 2: bad arguments or malformed input
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"mediarun/internal/model"
	"mediarun/internal/pipeline"
	"mediarun/internal/testkit"
	"mediarun/internal/txn"
)

const usage = `usage: mediarun <demo|verify> [flags]

  demo    generate fixtures, run the pipeline and commit artifacts
  verify  re-read a previous run's artifacts and check every segment hash

demo flags:
  --seed int           fixture seed (default 42)
  --packet-loss name   clean|reorder|duplicate|burst|wrap (default burst)
  --bandwidth list     comma-separated Mbps samples (default 2,8,1)
  --window int         reorder window (default 64)
  --out path           artifact directory (default ./artifacts)

verify flags:
  --dir path           artifact directory (default ./artifacts)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "mediarun: unknown command %q\n%s", args[0], usage)
		return 2
	}
}

func runDemo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Int64("seed", 42, "fixture seed")
	loss := fs.String("packet-loss", "burst", "defect pattern")
	bandwidth := fs.String("bandwidth", "2,8,1", "bandwidth samples in Mbps")
	window := fs.Int("window", 64, "reorder window")
	out := fs.String("out", "./artifacts", "artifact directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "mediarun: unexpected arguments %v\n", fs.Args())
		return 2
	}
	scenario, err := testkit.ParseScenario(*loss)
	if err != nil {
		fmt.Fprintf(stderr, "mediarun: %v\n", err)
		return 2
	}
	bands, err := parseBandwidth(*bandwidth)
	if err != nil {
		fmt.Fprintf(stderr, "mediarun: %v\n", err)
		return 2
	}
	result, err := pipeline.Run(pipeline.Options{
		Seed:      *seed,
		Scenario:  scenario,
		Bandwidth: bands,
		Out:       *out,
		Window:    *window,
	})
	if err != nil {
		fmt.Fprintf(stderr, "mediarun: demo failed: %v\n", err)
		if isInputError(err) {
			return 2
		}
		return 1
	}
	printResult(stdout, result)
	if failed := assertResult(result); len(failed) > 0 {
		fmt.Fprintf(stderr, "mediarun: assertions failed: %s\n", strings.Join(failed, ", "))
		return 1
	}
	return 0
}

func runVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "./artifacts", "artifact directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "mediarun: unexpected arguments %v\n", fs.Args())
		return 2
	}
	manifest, err := txn.LoadManifest(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "mediarun: verify: %v\n", err)
		return 2
	}
	if len(manifest.Segments) == 0 {
		fmt.Fprintf(stderr, "mediarun: verify: no committed segments in %s\n", *dir)
		return 2
	}
	report, err := txn.VerifyManifest(*dir, manifest)
	if err != nil {
		fmt.Fprintf(stderr, "mediarun: verify: %v\n", err)
		return 2
	}
	names := make([]string, 0, len(report.Results))
	for _, r := range report.Results {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	for _, r := range report.Results {
		status := "ok"
		if r.Missing {
			status = "missing"
		} else if !r.OK {
			status = "mismatch"
		}
		fmt.Fprintf(stdout, "segment=%s status=%s sha256=%s\n", r.Name, status, r.Actual)
	}
	rec, err := txn.Recover(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "mediarun: verify: recovery: %v\n", err)
		return 1
	}
	// Re-read after recovery: recovery must keep every committed hash.
	after, err := txn.LoadManifest(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "mediarun: verify: %v\n", err)
		return 1
	}
	if !txn.CompareHashes(manifest, after) {
		fmt.Fprintln(stderr, "mediarun: verify: recovery changed committed hashes")
		return 1
	}
	recovered := 0
	if rec.Committed > 0 && rec.HashMismatch == 0 {
		recovered = 1
	}
	match := 0
	if rec.HashMismatch == 0 && report.AllOK() {
		match = 1
	}
	fmt.Fprintf(stdout, "segments=%d\n", len(report.Results))
	fmt.Fprintf(stdout, "recovered=%d\n", recovered)
	fmt.Fprintf(stdout, "committed_hash_match=%d\n", match)
	fmt.Fprintf(stdout, "gaps=%d\n", countGaps(manifest))
	if !report.AllOK() || match != 1 || recovered != 1 {
		fmt.Fprintln(stderr, "mediarun: verify: verification mismatch")
		return 1
	}
	return 0
}

func countGaps(m *txn.Manifest) int {
	total := 0
	for _, s := range m.Segments {
		total += s.Gaps
	}
	return total
}

func printResult(w io.Writer, r *pipeline.Result) {
	fmt.Fprintf(w, "roundtrip_rate=%.3f\n", r.RoundTripRate)
	fmt.Fprintf(w, "keyframe_hit_rate=%.3f\n", r.KeyframeRate)
	fmt.Fprintf(w, "segments=%d\n", r.Segments)
	fmt.Fprintf(w, "events=%d\n", r.Events)
	fmt.Fprintf(w, "frames=%d\n", r.Frames)
	fmt.Fprintf(w, "detected_gaps=%d\n", r.DetectedGaps)
	fmt.Fprintf(w, "gap_scope=%s\n", r.GapSummary)
	fmt.Fprintf(w, "corrupt_cases=%d\n", r.CorruptCases)
	fmt.Fprintf(w, "corrupt_rejected=%d\n", r.CorruptCaught)
	fmt.Fprintf(w, "rollback_count=%d\n", r.RollbackCount)
	fmt.Fprintf(w, "recovered=%d\n", r.Recovered)
	fmt.Fprintf(w, "committed_hash_match=%d\n", r.CommittedMatch)
	for i, h := range r.SegmentHashes {
		fmt.Fprintf(w, "segment_hashes[%d]=%s\n", i, h)
	}
}

// assertResult checks the acceptance thresholds independent of display.
func assertResult(r *pipeline.Result) []string {
	var failed []string
	if r.RoundTripRate < 0.999999 {
		failed = append(failed, "roundtrip_rate")
	}
	if r.KeyframeRate < 0.999999 {
		failed = append(failed, "keyframe_hit_rate")
	}
	if r.Segments < 3 {
		failed = append(failed, "segments")
	}
	if len(r.SegmentHashes) != r.Segments {
		failed = append(failed, "segment_hashes")
	}
	for _, h := range r.SegmentHashes {
		if len(h) != 64 {
			failed = append(failed, "segment_hash_length")
			break
		}
	}
	if r.RollbackCount < 1 {
		failed = append(failed, "rollback_count")
	}
	if r.CorruptCases == 0 || r.CorruptCaught != r.CorruptCases {
		failed = append(failed, "corrupt_rejected")
	}
	// Only a lossy pattern must surface a hole; reordering, duplication
	// and wraparound are recoverable without one.
	if r.Scenario == "burst" && r.DetectedGaps <= 0 {
		failed = append(failed, "detected_gaps")
	}
	if r.Recovered != 1 {
		failed = append(failed, "recovered")
	}
	if r.CommittedMatch != 1 {
		failed = append(failed, "committed_hash_match")
	}
	return failed
}

func parseBandwidth(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	if len(parts) == 0 {
		return nil, fmt.Errorf("%w: empty bandwidth", model.ErrInvalid)
	}
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("%w: empty bandwidth sample", model.ErrInvalid)
		}
		v, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("%w: bandwidth %q: %v", model.ErrInvalid, p, err)
		}
		if v < 1 || v > 10000 {
			return nil, fmt.Errorf("%w: bandwidth %d out of range", model.ErrInvalid, v)
		}
		out = append(out, v)
	}
	return out, nil
}

func isInputError(err error) bool {
	if model.ClassOf(err) == model.ClassInvalid {
		return true
	}
	var pathErr *os.PathError
	return errors.As(err, &pathErr)
}
