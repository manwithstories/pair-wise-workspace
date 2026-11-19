// Command aircheck inspects MP4/MOV program masters, checks streams and
// keyframe intervals, remuxes onto a unified timeline and verifies output
// with RTP loss simulation. Terminal-only; no network access.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"aircheck/internal/clock"
	"aircheck/internal/fault"
	"aircheck/internal/pipe"
	"aircheck/internal/remux"
	"aircheck/internal/sample"
	"aircheck/internal/scan"
	"aircheck/internal/verify"
)

// Exit codes: 0 batch ok, 1 corrupt input or verify failure, 2 bad
// arguments or environment, 130 canceled by signal.
const (
	exitOK      = 0
	exitFailed  = 1
	exitUsage   = 2
	exitCanceled = 130
)

const keyframeGapThreshold = 12

type config struct {
	queue        int
	consumerDelay time.Duration
	loss         float64
	seed         int64
	jobs         int
	jsonOut      bool
	outDir       string
}

type fileResult struct {
	File              string     `json:"file"`
	Status            string     `json:"status"` // ok, recovered, failed
	Tracks            int        `json:"tracks"`
	Packets           int        `json:"packets"`
	AVSyncErrorMS     float64    `json:"av_sync_error_ms"`
	DamageOffset      int64      `json:"damage_offset"`
	RecoveredSegments int        `json:"recovered_segments"`
	KeyframeGaps      int        `json:"keyframe_gaps"`
	LossRecoveryRate  float64    `json:"loss_recovery_rate"`
	P95LatencyMS      float64    `json:"p95_latency_ms"`
	Faults            fault.List `json:"faults,omitempty"`
}

type summary struct {
	Files             int          `json:"files"`
	Passed            int          `json:"passed"`
	Failed            int          `json:"failed"`
	AVSyncErrorMS     float64      `json:"av_sync_error_ms"`
	LossRecoveryRate  float64      `json:"loss_recovery_rate"`
	P95LatencyMS      float64      `json:"p95_latency_ms"`
	DamageOffset      int64        `json:"damage_offset"`
	RecoveredSegments int          `json:"recovered_segments"`
	Status            string       `json:"status"`
	Results           []fileResult `json:"results"`
}

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) < 2 {
		usage()
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var code int
	switch os.Args[1] {
	case "demo":
		code = cmdDemo(ctx, os.Args[2:])
	case "scan":
		code = cmdScan(ctx, os.Args[2:])
	default:
		usage()
		return exitUsage
	}
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "canceled by signal")
		return exitCanceled
	}
	return code
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  aircheck demo [--seed N] [--queue N] [--consumer-delay D] [--loss F] [--json] [--out DIR]
  aircheck scan [--jobs N] [--queue N] [--loss F] [--json] [--out DIR] FILE...`)
}

func newFlagSet(name string, cfg *config) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.IntVar(&cfg.queue, "queue", 8, "bounded queue capacity")
	fs.DurationVar(&cfg.consumerDelay, "consumer-delay", 0, "artificial per-packet consumer delay")
	fs.Float64Var(&cfg.loss, "loss", 0.0, "RTP loss injection rate [0,1]")
	fs.Int64Var(&cfg.seed, "seed", 1, "deterministic seed")
	fs.IntVar(&cfg.jobs, "jobs", 2, "max concurrent files")
	fs.BoolVar(&cfg.jsonOut, "json", false, "emit JSON summary")
	fs.StringVar(&cfg.outDir, "out", "", "output directory (default: temp dir)")
	return fs
}

func cmdDemo(ctx context.Context, args []string) int {
	cfg := &config{seed: 1}
	if err := newFlagSet("demo", cfg).Parse(args); err != nil {
		return exitUsage
	}
	work, err := os.MkdirTemp("", "aircheck-demo-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	defer os.RemoveAll(work)
	files, err := sample.Generate(cfg.seed, work)
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate samples:", err)
		return exitUsage
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	return runBatch(ctx, cfg, paths)
}

func cmdScan(ctx context.Context, args []string) int {
	cfg := &config{seed: 1}
	fs := newFlagSet("scan", cfg)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() == 0 {
		usage()
		return exitUsage
	}
	return runBatch(ctx, cfg, fs.Args())
}

// runBatch processes files with a concurrency cap; one file's failure
// never hides another's diagnostics.
func runBatch(ctx context.Context, cfg *config, paths []string) int {
	out := cfg.outDir
	if out == "" {
		var err error
		out, err = os.MkdirTemp("", "aircheck-out-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitUsage
		}
		defer os.RemoveAll(out)
	}

	results := make([]fileResult, len(paths))
	sem := make(chan struct{}, cfg.jobs)
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = processFile(ctx, cfg, p, out)
		}(i, p)
	}
	wg.Wait()

	sum := summarize(results)
	if cfg.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(sum)
	} else {
		for _, r := range results {
			fmt.Printf("%s: %s (tracks=%d packets=%d av_sync=%.1fms p95=%.1fms damage_offset=%d)\n",
				r.File, r.Status, r.Tracks, r.Packets, r.AVSyncErrorMS, r.P95LatencyMS, r.DamageOffset)
		}
		fmt.Printf("files=%d passed=%d failed=%d status=%s\n", sum.Files, sum.Passed, sum.Failed, sum.Status)
	}
	if sum.Failed > 0 {
		return exitFailed
	}
	return exitOK
}

func summarize(results []fileResult) summary {
	sum := summary{Files: len(results), Status: "ok", DamageOffset: -1}
	for _, r := range results {
		sum.Results = append(sum.Results, r)
		switch r.Status {
		case "failed":
			sum.Failed++
			sum.Status = "failed"
		default:
			sum.Passed++
		}
		if r.AVSyncErrorMS > sum.AVSyncErrorMS {
			sum.AVSyncErrorMS = r.AVSyncErrorMS
		}
		if r.P95LatencyMS > sum.P95LatencyMS {
			sum.P95LatencyMS = r.P95LatencyMS
		}
		if r.DamageOffset >= 0 && (sum.DamageOffset < 0 || r.DamageOffset < sum.DamageOffset) {
			sum.DamageOffset = r.DamageOffset
		}
		sum.RecoveredSegments += r.RecoveredSegments
	}
	lost, recovered := lossTotals()
	if lost > 0 {
		sum.LossRecoveryRate = float64(recovered) * 100 / float64(lost)
	}
	return sum
}

// aggregate loss counters protected for concurrent file workers.
var (
	lossMu         sync.Mutex
	lossCount      int
	recoveredCount int
)

func addLoss(lost, recovered int) {
	lossMu.Lock()
	lossCount += lost
	recoveredCount += recovered
	lossMu.Unlock()
}

func lossTotals() (lost, recovered int) {
	lossMu.Lock()
	defer lossMu.Unlock()
	return lossCount, recoveredCount
}

// processFile runs the full pipeline for one input file.
func processFile(ctx context.Context, cfg *config, path, outDir string) fileResult {
	res := fileResult{File: path, Status: "ok", DamageOffset: -1}
	fail := func(f fault.Fault) {
		res.Faults = append(res.Faults, f)
		res.Status = "failed"
	}

	f, err := os.Open(path)
	if err != nil {
		fail(fault.New(fault.KindUnrecoverable, fault.StageScan, 0, 0, -1, false, err.Error()))
		return res
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		fail(fault.New(fault.KindUnrecoverable, fault.StageScan, 0, 0, -1, false, err.Error()))
		return res
	}

	ci, err := scan.Parse(f, st.Size())
	if err != nil {
		fail(fault.New(fault.KindUnrecoverable, fault.StageScan, 0, 0, ci.DamageOffset, false, err.Error()))
		return res
	}
	res.Tracks = len(ci.Tracks)
	res.DamageOffset = ci.DamageOffset
	res.Faults = append(res.Faults, ci.Faults...)

	byTrack := map[uint32][]scan.Packet{}
	var all []scan.Packet
	for _, t := range ci.Tracks {
		pkts, fl := scan.BuildPackets(t, st.Size())
		res.Faults = append(res.Faults, fl...)
		byTrack[t.ID] = pkts
		all = append(all, pkts...)
		res.Packets += len(pkts)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].DTS < all[j].DTS })

	// Keyframe interval check on the video track.
	for _, t := range ci.Tracks {
		if t.MediaType != "video" {
			continue
		}
		gaps := scan.KeyframeGaps(byTrack[t.ID], keyframeGapThreshold)
		res.KeyframeGaps += len(gaps)
		for _, g := range gaps {
			res.Faults = append(res.Faults, fault.New(fault.KindKeyframeGap, fault.StageScan,
				t.ID, g.StartTime, g.StartOffset, true,
				fmt.Sprintf("keyframe gap %d exceeds %d", g.Length, keyframeGapThreshold)))
		}
	}

	// A/V sync from first unified timestamps.
	res.AVSyncErrorMS = avSync(ci, byTrack)

	// Unrecoverable damage: keep diagnostics, never silently skip.
	if uf := fault.List(res.Faults).Unrecoverable(); uf != nil {
		res.Status = "failed"
		return res
	}

	// Pipeline: bounded queue, backpressure, consumer delay.
	runner := &pipe.Runner{Capacity: cfg.queue, ConsumerDelay: cfg.consumerDelay, SlowTimeout: 10 * time.Second}
	stats, _, qfaults := runner.Run(ctx, all, func(context.Context, pipe.Item) error { return nil })
	res.Faults = append(res.Faults, qfaults...)
	res.P95LatencyMS = float64(stats.P95Latency()) / float64(time.Millisecond)
	if len(qfaults) > 0 {
		res.Status = "failed"
		return res
	}

	// Remux onto the unified timeline.
	mr, err := remux.Remux(ctx, f, ci, byTrack, outDir, remux.Policy{DropTruncatedTail: true, AssumeAllSync: true})
	if err != nil {
		if flt, ok := err.(fault.Fault); ok {
			res.Faults = append(res.Faults, flt)
		}
		res.Status = "failed"
		return res
	}
	res.Faults = append(res.Faults, mr.Faults...)
	res.RecoveredSegments = mr.RecoveredSegments

	// Verify: re-parse output and compare digests before finalizing.
	outCI, outByTrack, verr := reparse(mr, len(all))
	if verr != nil {
		mr.Abandon()
		fail(fault.New(fault.KindVerifyMismatch, fault.StageVerify, 0, 0, -1, false, verr.Error()))
		return res
	}
	want := verify.Summarize(ci, byTrack)
	if vf := verify.CompareOutput(want, outCI, outByTrack); len(vf) > 0 {
		mr.Abandon()
		res.Faults = append(res.Faults, vf...)
		res.Status = "failed"
		return res
	}
	if err := mr.Finalize(base(path) + ".aircheck.mp4"); err != nil {
		fail(fault.New(fault.KindWriteFailure, fault.StageWrite, 0, 0, -1, false, err.Error()))
		return res
	}

	// RTP loss injection and recovery, starting near sequence wrap.
	if err := verify.RoundTrip(65500, 200, 0x00A14C00); err != nil {
		fail(fault.New(fault.KindVerifyMismatch, fault.StageVerify, 0, 0, -1, false, err.Error()))
		return res
	}
	lr := verify.SimulateLoss(res.Packets, 65500, cfg.loss, cfg.seed, 4)
	res.LossRecoveryRate = lr.RecoveryRate
	addLoss(lr.Lost, lr.Recovered)

	if len(res.Faults) > 0 {
		res.Status = "recovered"
	}
	return res
}

func avSync(ci *scan.ContainerInfo, byTrack map[uint32][]scan.Packet) float64 {
	var aFirst, vFirst int64
	var haveA, haveV bool
	for _, t := range ci.Tracks {
		pkts := byTrack[t.ID]
		if len(pkts) == 0 {
			continue
		}
		switch t.MediaType {
		case "audio":
			if !haveA || pkts[0].PTS < aFirst {
				aFirst = pkts[0].PTS
			}
			haveA = true
		case "video":
			if !haveV || pkts[0].PTS < vFirst {
				vFirst = pkts[0].PTS
			}
			haveV = true
		}
	}
	if !haveA || !haveV {
		return 0
	}
	d := aFirst - vFirst
	if d < 0 {
		d = -d
	}
	return clock.ToMillis(d)
}

func reparse(mr *remux.Result, _ int) (*scan.ContainerInfo, map[uint32][]scan.Packet, error) {
	path := mr.OutputPath
	if path == "" {
		path = mr.TmpPath()
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	ci, err := scan.Parse(f, st.Size())
	if err != nil {
		return nil, nil, err
	}
	byTrack := map[uint32][]scan.Packet{}
	for _, t := range ci.Tracks {
		pkts, _ := scan.BuildPackets(t, st.Size())
		byTrack[t.ID] = pkts
	}
	return ci, byTrack, nil
}

func base(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
