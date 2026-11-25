// Command mediad runs the media processing kernel demo. It resolves local
// manifests, adapts segments to unified samples, optionally recovers RTP, and
// writes verified lossless output. Exit status: 0 success, 2 quarantined
// damaged input, 1 usage/I/O/internal failure.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mediad/internal/adapter"
	"mediad/internal/fixture"
	"mediad/internal/media"
	"mediad/internal/pipeline"
	"mediad/internal/timeline"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "demo":
		os.Exit(runDemo(os.Args[2:]))
	case "generate":
		os.Exit(runGenerate(os.Args[2:]))
	case "verify":
		os.Exit(runVerify(os.Args[2:]))
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
	fmt.Fprint(os.Stderr, `mediad — local media processing kernel demo

usage:
  mediad demo    --seed N --profile clean|recover|corrupt [flags]
  mediad generate --seed N --profile P --out DIR [flags]
  mediad verify  --dir DIR

demo flags:
  --seed N            deterministic fixture seed
  --profile P         clean | recover | corrupt
  --cams N            camera count (default 4)
  --segments N        segments per camera (default 180)
  --loss F            RTP loss fraction for the recover profile
  --drift-ppm F       per-camera clock drift ceiling
  --wrap-pts N        shift PTS by N ticks to exercise 33-bit wrap
  --vfr               emit variable frame durations
  --truncate-moof N   bytes to cut from the corrupt segment
  --container C       cmaf (default)
  --work DIR          override the working directory
  --out DIR           override the output directory
`)
}

type demoFlags struct {
	seed      int64
	profile   string
	cams      int
	segments  int
	loss      float64
	driftPPM  float64
	wrapPTS   int64
	vfr       bool
	truncate  int64
	container string
	work      string
	out       string
}

func parseDemoFlags(args []string) (*demoFlags, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	d := &demoFlags{}
	fs.Int64Var(&d.seed, "seed", 41, "fixture seed")
	fs.StringVar(&d.profile, "profile", "clean", "clean|recover|corrupt")
	fs.IntVar(&d.cams, "cams", 4, "camera count")
	fs.IntVar(&d.segments, "segments", 180, "segments per camera")
	fs.Float64Var(&d.loss, "loss", 0.03, "RTP loss fraction")
	fs.Float64Var(&d.driftPPM, "drift-ppm", 120, "clock drift ceiling in ppm")
	fs.Int64Var(&d.wrapPTS, "wrap-pts", 0, "PTS shift to exercise 33-bit wrap")
	fs.BoolVar(&d.vfr, "vfr", false, "emit variable frame durations")
	fs.Int64Var(&d.truncate, "truncate-moof", 0, "bytes to cut from the corrupt segment")
	fs.StringVar(&d.container, "container", "cmaf", "container adapter")
	fs.StringVar(&d.work, "work", "", "working directory")
	fs.StringVar(&d.out, "out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	switch d.profile {
	case "clean", "recover", "corrupt":
	default:
		return nil, fmt.Errorf("unknown profile %q", d.profile)
	}
	if d.cams <= 0 || d.segments <= 0 {
		return nil, fmt.Errorf("cams and segments must be positive")
	}
	return d, nil
}

func runDemo(args []string) int {
	d, err := parseDemoFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediad demo: %v\n", err)
		return 1
	}
	work := d.work
	if work == "" {
		work, err = os.MkdirTemp("", "mediad-demo-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "mediad demo: temp dir: %v\n", err)
			return 1
		}
		defer os.RemoveAll(work)
	} else {
		if err := os.MkdirAll(work, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "mediad demo: work dir: %v\n", err)
			return 1
		}
	}
	out := d.out
	if out == "" {
		out = filepath.Join(work, "out")
	}

	fixtureDir := filepath.Join(work, "fixture")
	profile := fixture.Profile(d.profile)
	info, err := fixture.Generate(fixture.Options{
		Seed: d.seed, Profile: profile, Cams: d.cams, Segments: d.segments,
		Root: fixtureDir, Loss: d.loss, DriftPPM: d.driftPPM, WrapPTS: d.wrapPTS,
		VFR: d.vfr, TruncateMoof: d.truncate, Container: d.container,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediad demo: generate: %v\n", err)
		return 1
	}

	cfg := pipeline.Config{
		Root:      fixtureDir,
		OutDir:    out,
		Container: adapter.Container(d.container),
		Align:     timeline.AlignOptions{},
		Limits:    media.DefaultLimits(),
	}
	if profile == fixture.ProfileRecover {
		cfg.Mode = pipeline.ModeRTP
	}
	rep, runErr := pipeline.Run(cfg)
	if rep != nil {
		rep.Profile = d.profile
		rep.Seed = d.seed
	}
	printReport(rep)

	if runErr != nil {
		if media.Isolated(runErr) {
			return 2
		}
		return 1
	}
	if err := checkInvariants(d, info, rep); err != nil {
		fmt.Fprintf(os.Stderr, "mediad demo: invariant violated: %v\n", err)
		printJSON(map[string]any{"ok": false, "error": err.Error()})
		return 1
	}
	printJSON(map[string]any{"ok": true, "profile": d.profile, "output": filepath.Join(out, "output.mp4")})
	return 0
}

// checkInvariants asserts the contract for each demo profile.
func checkInvariants(d *demoFlags, info *fixture.Info, rep *pipeline.Report) error {
	if rep == nil {
		return fmt.Errorf("no report produced")
	}
	switch fixture.Profile(d.profile) {
	case fixture.ProfileClean:
		if rep.SyncP95MS > 20 {
			return fmt.Errorf("sync_p95_ms=%.2f exceeds 20", rep.SyncP95MS)
		}
		if !rep.HashMatch {
			return fmt.Errorf("hash_match=false on clean run")
		}
		if rep.OutputSamples != info.Expected.TotalSamples {
			return fmt.Errorf("output_samples=%d want %d", rep.OutputSamples, info.Expected.TotalSamples)
		}
	case fixture.ProfileRecover:
		if rep.LossRecoveryPct < 95 {
			return fmt.Errorf("loss_recovery_pct=%.2f below 95", rep.LossRecoveryPct)
		}
		if rep.RecoveredPackets <= 0 {
			return fmt.Errorf("recovered_packets=0")
		}
		if rep.LatencyP95MS > 100 {
			return fmt.Errorf("latency_p95_ms=%.2f exceeds 100", rep.LatencyP95MS)
		}
	case fixture.ProfileCorrupt:
		if rep.RejectedSegments != 1 {
			return fmt.Errorf("rejected_segments=%d want 1", rep.RejectedSegments)
		}
	}
	return nil
}

func runGenerate(args []string) int {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := fs.Int64("seed", 41, "seed")
	profile := fs.String("profile", "clean", "clean|recover|corrupt")
	cams := fs.Int("cams", 4, "cameras")
	segments := fs.Int("segments", 180, "segments")
	loss := fs.Float64("loss", 0.03, "loss fraction")
	drift := fs.Float64("drift-ppm", 120, "drift ppm")
	wrap := fs.Int64("wrap-pts", 0, "wrap pts")
	vfr := fs.Bool("vfr", false, "vfr")
	trunc := fs.Int64("truncate-moof", 17, "truncate bytes")
	container := fs.String("container", "cmaf", "container")
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "mediad generate: %v\n", err)
		return 1
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "mediad generate: --out is required")
		return 1
	}
	info, err := fixture.Generate(fixture.Options{
		Seed: *seed, Profile: fixture.Profile(*profile), Cams: *cams, Segments: *segments,
		Root: *out, Loss: *loss, DriftPPM: *drift, WrapPTS: *wrap, VFR: *vfr,
		TruncateMoof: *trunc, Container: *container,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediad generate: %v\n", err)
		return 1
	}
	printJSON(info)
	return 0
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", "", "fixture directory")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	info, err := fixture.ReadInfo(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediad verify: %v\n", err)
		return 1
	}
	printJSON(info.Expected)
	return 0
}

func printReport(rep *pipeline.Report) {
	if rep == nil {
		return
	}
	fmt.Printf("output_samples=%d output_tracks=%d\n", rep.OutputSamples, rep.OutputTracks)
	fmt.Printf("sync_p95_ms=%.3f sync_max_ms=%.3f\n", rep.SyncP95MS, rep.SyncMaxMS)
	fmt.Printf("segment_p95_ms=%.3f segment_p99_ms=%.3f\n", rep.SegmentP95MS, rep.SegmentP99MS)
	fmt.Printf("latency_p95_ms=%.3f\n", rep.LatencyP95MS)
	fmt.Printf("lost_packets=%d unrecoverable_packets=%d duplicate_packets=%d recovered_packets=%d\n",
		rep.LostPackets, rep.Unrecoverable, rep.DuplicatePackets, rep.RecoveredPackets)
	fmt.Printf("loss_recovery_pct=%.2f\n", rep.LossRecoveryPct)
	fmt.Printf("rejected_segments=%d segments=%d\n", rep.RejectedSegments, rep.Segments)
	for _, f := range rep.Fits {
		fmt.Printf("drift camera=%s slope_ppm=%.2f anchor_ref=%d anchor_local=%d boundary_err_ms=%.3f jumps=%d\n",
			f.Camera, f.SlopePPM, f.AnchorRef, f.AnchorLocal, f.BoundaryErrMS, f.Jumps)
	}
	fmt.Printf("hash_match=%t output_sha256=%s\n", rep.HashMatch, rep.OutputSHA256)
	if rep.ErrCode != "" {
		fmt.Printf("err_code=%s\n", rep.ErrCode)
		for _, e := range rep.Errors {
			fmt.Printf("error=%s\n", strings.TrimSpace(e))
		}
	}
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
