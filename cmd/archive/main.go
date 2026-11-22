// Command archive restores decades of HLS/DASH archive material:
// it verifies manifests, demuxes segments, remuxes losslessly into a
// content-addressed store, checkpoints every segment atomically and
// reports independently verified metrics. It is terminal-only.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"archive/internal/fault"
	"archive/internal/fixture"
	"archive/internal/metrics"
	"archive/internal/pipeline"
	"archive/internal/rtp"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return pipeline.ExitFailure
	}
	switch args[0] {
	case "demo":
		return cmdDemo(args[1:])
	case "resume":
		return cmdResume(args[1:])
	case "help", "-h", "--help":
		usage()
		return pipeline.ExitOK
	default:
		fmt.Fprintf(os.Stderr, "archive: unknown command %q\n", args[0])
		usage()
		return pipeline.ExitFailure
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  archive demo   --root DIR [--segments N] [--loss-every N]
                 [--interrupt-after N] [--corrupt KIND]
  archive resume --root DIR

demo generates a synthetic archive under DIR, runs the pipeline and
prints its root for resume. resume continues at the first uncommitted
segment. Exit 0 = complete, 2 = damaged input but confirmed segments
kept, 1 = verification, storage or recovery failure.
`)
}

type demoOpts struct {
	root           string
	segments       int
	lossEvery      int
	interruptAfter int
	corrupt        string
}

func cmdDemo(args []string) int {
	var o demoOpts
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.StringVar(&o.root, "root", "", "archive root directory (required)")
	fs.IntVar(&o.segments, "segments", 120, "number of synthetic segments")
	fs.IntVar(&o.lossEvery, "loss-every", 0, "drop one RTP packet every N (0 = no loss)")
	fs.IntVar(&o.interruptAfter, "interrupt-after", -1, "stop after N newly committed segments; 0 or negative runs to completion")
	fs.StringVar(&o.corrupt, "corrupt", "", "damage kind: truncated|missing|duplicate|wrap|conflict")
	if err := fs.Parse(args); err != nil {
		return pipeline.ExitFailure
	}
	if o.root == "" {
		fmt.Fprintln(os.Stderr, "archive demo: --root is required")
		return pipeline.ExitFailure
	}
	if o.segments <= 0 {
		fmt.Fprintln(os.Stderr, "archive demo: --segments must be positive")
		return pipeline.ExitFailure
	}
	opt, err := corruptOptions(o.corrupt, o.segments)
	if err != nil {
		return fail(err)
	}
	opt.Segments = o.segments
	lay, err := fixture.Generate(o.root, opt)
	if err != nil {
		return fail(err)
	}
	// Record the drill parameters so resume reproduces the same RTP
	// metrics as the original demo run.
	if err := saveState(o); err != nil {
		return fail(err)
	}
	fmt.Printf("root=%s\n", lay.Root)
	fmt.Printf("corrupt=%s segments=%d\n", orNone(o.corrupt), o.segments)

	res, err := pipeline.Run(pipeline.Config{
		Root:           o.root,
		Manifest:       lay.HLS,
		InterruptAfter: o.interruptAfter,
	})
	if err != nil {
		return fail(err)
	}
	printPipeline(res)

	rep, err := metrics.Build(o.root, res.Latencies)
	if err != nil {
		return fail(err)
	}
	rtpN, inj := rtpPlan(o.segments, o.lossEvery)
	rtpRes, err := rtp.Recover(rtpN, rtp.Build(rtpN, inj))
	if err != nil {
		return fail(err)
	}
	printReport(rep, rtpRes)
	if err := metrics.WriteJSON(o.root, rep); err != nil {
		return fail(err)
	}
	if res.Exit == pipeline.ExitRecovered {
		fmt.Printf("first_bad_segment=%s recoverable_segments=%d\n", res.FirstBad, len(res.Entries))
	}
	return res.Exit
}

// corruptOptions maps a damage kind to fixture options. Damage is
// placed mid-stream when possible so the run can show confirmed
// segments before the first bad dependency.
func corruptOptions(kind string, segments int) (fixture.Options, error) {
	none := -1
	mid := segments / 2
	if mid < 1 {
		mid = 1
	}
	o := fixture.Options{TruncateAt: none, MissingAt: none, DuplicateAt: none, ConflictAt: none}
	switch kind {
	case "":
	case "truncated":
		o.TruncateAt = mid
	case "missing":
		o.MissingAt = mid
	case "duplicate":
		o.DuplicateAt = mid
	case "wrap":
		o.Wrap = true
	case "conflict":
		o.ConflictAt = mid
	default:
		return o, fault.Newf(fault.Fatal, "demo", "", "unknown corrupt kind %q", kind)
	}
	return o, nil
}

func cmdResume(args []string) int {
	var root string
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.StringVar(&root, "root", "", "archive root from demo (required)")
	if err := fs.Parse(args); err != nil {
		return pipeline.ExitFailure
	}
	if root == "" {
		fmt.Fprintln(os.Stderr, "archive resume: --root is required")
		return pipeline.ExitFailure
	}
	manifest := "media.m3u8"
	if _, err := os.Stat(filepath.Join(root, manifest)); err != nil {
		fmt.Fprintln(os.Stderr, "archive resume: no manifest at root; run demo first")
		return pipeline.ExitFailure
	}
	state, err := loadState(root)
	if err != nil {
		return fail(err)
	}
	res, err := pipeline.Run(pipeline.Config{Root: root, Manifest: manifest, InterruptAfter: -1})
	if err != nil {
		return fail(err)
	}
	fmt.Printf("root=%s\n", root)
	printPipeline(res)
	rep, err := metrics.Build(root, res.Latencies)
	if err != nil {
		return fail(err)
	}
	rtpN, inj := rtpPlan(state.Segments, state.LossEvery)
	rtpRes, err := rtp.Recover(rtpN, rtp.Build(rtpN, inj))
	if err != nil {
		return fail(err)
	}
	printReport(rep, rtpRes)
	if err := metrics.WriteJSON(root, rep); err != nil {
		return fail(err)
	}
	if res.Exit == pipeline.ExitRecovered {
		fmt.Printf("first_bad_segment=%s recoverable_segments=%d\n", res.FirstBad, len(res.Entries))
	}
	return res.Exit
}

// runState persists the demo parameters needed for resume to reproduce
// the same RTP drill without new flags.
type runState struct {
	Segments  int    `json:"segments"`
	LossEvery int    `json:"loss_every"`
	Corrupt   string `json:"corrupt,omitempty"`
}

func statePath(root string) string { return filepath.Join(root, "work", "demo.json") }

func saveState(o demoOpts) error {
	raw, err := json.Marshal(runState{Segments: o.segments, LossEvery: o.lossEvery, Corrupt: o.corrupt})
	if err != nil {
		return fault.Wrap(fault.Fatal, "demo", o.root, err)
	}
	dir := filepath.Join(o.root, "work")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fault.Wrap(fault.Fatal, "demo", dir, err)
	}
	tmp := filepath.Join(dir, "demo.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fault.Wrap(fault.Fatal, "demo", tmp, err)
	}
	return os.Rename(tmp, statePath(o.root))
}

// loadState tolerates a missing file: an archive from an older demo
// still resumes, just without the RTP drill parameters.
func loadState(root string) (runState, error) {
	raw, err := os.ReadFile(statePath(root))
	if os.IsNotExist(err) {
		return runState{}, nil
	}
	if err != nil {
		return runState{}, fault.Wrap(fault.Fatal, "resume", root, err)
	}
	var st runState
	if err := json.Unmarshal(raw, &st); err != nil {
		return runState{}, fault.Wrap(fault.Fatal, "resume", root, err)
	}
	return st, nil
}

// rtpPlan chooses the synthetic RTP sequence size and faults. The
// stream is kept small enough that both a reordered packet and a lost
// (then retransmitted) packet fit the repair window, so the demo always
// exercises recovery while still reporting real unrecoverable loss.
func rtpPlan(segments, lossEvery int) (int, rtp.Inject) {
	const maxStream = 200
	n := segments
	if n > maxStream {
		n = maxStream
	}
	if n < 1 {
		n = 1
	}
	// All three channel faults are always exercised: one duplicate and
	// a periodic reorder, plus the requested loss cadence. Losses are
	// repaired by a late retransmission, so they land inside the window.
	inj := rtp.Inject{DuplicateAt: 1, ReorderEvery: 7}
	if lossEvery > 0 && lossEvery < n {
		inj.DropEvery = lossEvery
	}
	return n, inj
}

func printPipeline(res pipeline.Result) {
	fmt.Printf("segments_seen=%d unique=%d committed=%d verified=%d duplicates=%d pending=%d\n",
		res.ManifestSeen, res.Unique, res.Committed, res.Verified, res.Duplicates, res.Pending)
}

func printReport(rep metrics.Report, rr rtp.Result) {
	fmt.Printf("tracks=%v\n", rep.TrackTable)
	fmt.Printf("sync_error_ms=%.1f recovered_packets=%d recovery_rate=%.1f p95_latency_ms=%.1f cache_verified=%d output_sha256=%s\n",
		rep.SyncErrorMs, rr.Recovered, rr.Rate, rep.P95LatencyMs, rep.CacheVerified, rep.OutputSHA)
	if rr.Sent > 0 {
		fmt.Printf("rtp sent=%d received=%d duplicates=%d recovered=%d reordered=%d unrecoverable=%d\n",
			rr.Sent, rr.Received, rr.Duplicates, rr.Recovered, rr.Reordered, rr.Unrecoverable)
	}
	for _, d := range rep.Desync {
		fmt.Printf("desync %s\n", d)
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "archive: %v\n", err)
	if fault.LevelOf(err) == fault.Fatal {
		return pipeline.ExitFailure
	}
	return pipeline.ExitRecovered
}
