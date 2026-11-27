// Command fieldcut turns three camera frame sets into one 25 fps timeline,
// exports the composited frames and proves audio/picture alignment. It is a
// terminal-only tool: every behaviour is reachable through flags.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"fieldcut/internal/gen"
	"fieldcut/internal/media"
	"fieldcut/internal/metrics"
	"fieldcut/internal/runner"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(1)
	}
	switch os.Args[1] {
	case "demo":
		os.Exit(demoCmd(os.Args[2:]))
	case "verify":
		os.Exit(verifyCmd(os.Args[2:]))
	case "version":
		os.Exit(0)
	case "-h", "--help", "help":
		usage(os.Stdout)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "fieldcut: unknown command %q\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `fieldcut - three-camera synthesis with audio/video alignment proof

usage:
  fieldcut demo   --seed N --frames N --tracks N --wrap-at N --drift-ppm N
                  --drop-every N --corrupt-every N --out DIR
  fieldcut verify --dir DIR
  fieldcut version

exit codes: 0 verified, 1 uncategorised, 2 input rejected,
            3 graph or time base invalid, 4 metrics out of bounds`)
}

// exitCode maps the shared error model onto the documented process codes.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var me *media.MediaError
	if errors.As(err, &me) {
		switch me.Kind {
		case media.KindInput, media.KindCorrupt:
			return 2
		case media.KindClock, media.KindGraph:
			return 3
		case media.KindMetric:
			return 4
		case media.KindConflict, media.KindLimit:
			return 2
		}
		return 1
	}
	return 1
}

func demoFlags(fs *flag.FlagSet) (*int64, *int, *int, *int64, *float64, *int, *int, *string) {
	seed := fs.Int64("seed", 41, "deterministic generator seed")
	frames := fs.Int("frames", 240, "output frame count")
	tracks := fs.Int("tracks", 3, "track count, at most 3")
	wrapAt := fs.Int64("wrap-at", 4294967200, "32-bit tick modulus for the wrap sample")
	drift := fs.Float64("drift-ppm", 120, "device clock drift in parts per million")
	dropEvery := fs.Int("drop-every", 37, "drop one frame every N sequences")
	corruptEvery := fs.Int("corrupt-every", 53, "corrupt one frame every N sequences")
	out := fs.String("out", "", "output directory, must be absolute or new")
	return seed, frames, tracks, wrapAt, drift, dropEvery, corruptEvery, out
}

func demoCmd(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed, frames, tracks, wrapAt, drift, dropEvery, corruptEvery, out := demoFlags(fs)
	selftest := fs.Bool("selftest", true, "run the rejection exit-code self-check")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "fieldcut demo: --out is required")
		return 2
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "fieldcut demo: %v\n", err)
		return 2
	}
	input := filepath.Join(*out, "input")
	if err := os.MkdirAll(input, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "fieldcut demo: %v\n", err)
		return 2
	}
	cfg := gen.Config{
		Seed: *seed, Frames: *frames, Tracks: *tracks, WrapAt: *wrapAt,
		DriftPPM: *drift, DropEvery: *dropEvery, CorruptEvery: *corruptEvery,
		Width: 320, Height: 180, Chunk: 32,
	}
	g, err := gen.Generate(input, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fieldcut demo: generate: %v\n", err)
		return exitCode(err)
	}
	fmt.Printf("generated frames=%d tracks=%d wrap_at=%d drift_ppm=%.0f corrupt=%d dropped=%d\n",
		cfg.Frames, cfg.Tracks, cfg.WrapAt, cfg.DriftPPM, countCorrupt(g), countMissing(g))

	opts := runner.Options{
		OutDir: filepath.Join(*out, "export"), InputDir: input,
		ConfigHash: cfg.ManifestHash(), Workers: 3,
	}
	// Interrupted run first: stop after a quarter of the frames, then resume.
	partialOpts := opts
	partialOpts.Limit = cfg.Frames / 4
	partial, err := runner.Run(partialOpts)
	resumedPartial := err == runner.ErrInterrupted
	if err != nil && !resumedPartial {
		fmt.Fprintf(os.Stderr, "fieldcut demo: interrupted run: %v\n", err)
		return exitCode(err)
	}
	if resumedPartial {
		fmt.Printf("interrupted after %d frames; resuming\n", cfg.Frames/4)
	}
	// Resume from the same directory with no limit: it must skip the
	// adopted frames and produce the same root hash as a clean run.
	final, err := runner.Run(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fieldcut demo: run: %v\n", err)
		return exitCode(err)
	}
	if partial != nil {
		fmt.Printf("partial frames=%d p95_frame_ms=%.0f\n", partial.Frames, partial.P95FrameMs)
	}

	// Clean re-run into a second directory to compare resume equality.
	pristine := runner.Options{
		OutDir: filepath.Join(*out, "export-full"), InputDir: input,
		ConfigHash: cfg.ManifestHash(), Workers: 3,
	}
	clean, err := runner.Run(pristine)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fieldcut demo: full run: %v\n", err)
		return exitCode(err)
	}
	v, err := runner.Verify(pristine.OutDir, input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fieldcut demo: verify: %v\n", err)
		return exitCode(err)
	}
	rep := metrics.Report{
		SyncMaxMs: final.SyncMaxMs, RecoveryRate: final.RecoveryRate,
		Eligible: final.Eligible, Recovered: final.Recovered,
		P95FrameMs: final.P95FrameMs, Frames: final.Frames,
		RootSHA256:  final.Root,
		ResumeEqual: final.Root == clean.Root,
		Verify:      "PASS",
	}
	if err := rep.Check(metrics.DefaultLimits); err != nil {
		fmt.Fprintln(os.Stderr, rep.Line())
		fmt.Fprintf(os.Stderr, "fieldcut demo: %v\n", err)
		return exitCode(err)
	}
	fmt.Println(rep.Line())
	fmt.Printf("export=%s verify_dir=%s audio_offset_ms=%.3f pulses=%d clean_root=%s\n",
		opts.OutDir, pristine.OutDir, float64(v.AudioOffsetNs)/1e6, v.Pulses, clean.Root)
	if *selftest {
		reportRejections(*out, input)
	}
	return 0
}

// reportRejections demonstrates the exit-code contract on throwaway copies
// of the generated input: each mutation must fail with its documented code.
func reportRejections(root, input string) {
	cases := []struct {
		name string
		kind string
		code int
	}{
		{"bad-hash", "bad-hash", 2},
		{"traversal", "traversal", 2},
		{"oversize", "oversize", 2},
		{"cycle", "cycle", 3},
		{"fps", "fps", 2},
		{"ambiguous-wrap", "ambiguous-wrap", 3},
		{"bad-frame", "bad-frame", 2},
	}
	for _, tc := range cases {
		dir := filepath.Join(root, "reject-"+tc.name)
		if err := copyInput(input, dir); err != nil {
			fmt.Printf("reject_check=%s setup_error=%v\n", tc.name, err)
			continue
		}
		if err := gen.Mutate(dir, tc.kind); err != nil {
			fmt.Printf("reject_check=%s mutate_error=%v\n", tc.name, err)
			continue
		}
		_, err := runner.Run(runner.Options{
			OutDir: filepath.Join(root, "reject-out-"+tc.name), InputDir: dir,
			ConfigHash: "reject-" + tc.name, Workers: 1,
		})
		code := exitCode(err)
		got := "ok"
		if code != tc.code {
			got = "MISMATCH"
		}
		fmt.Printf("reject_check=%s exit=%d want=%d %s\n", tc.name, code, tc.code, got)
	}
	// Metrics are pure post-processing, so their out-of-bounds code is
	// demonstrated without corrupting a dataset.
	bad := metrics.Report{SyncMaxMs: 9.9, RecoveryRate: 1.0, P95FrameMs: 10,
		Frames: 1, RootSHA256: "x", ResumeEqual: true, Verify: "PASS"}
	code := exitCode(bad.Check(metrics.DefaultLimits))
	fmt.Printf("reject_check=metrics exit=%d want=4 %s\n", code, matched(code, 4))
}

func matched(got, want int) string {
	if got == want {
		return "ok"
	}
	return "MISMATCH"
}

// copyInput mirrors the generated input so rejection cases never touch the
// real demo dataset.
func copyInput(src, dst string) error {
	entries, err := os.ReadDir(filepath.Join(src, "frames"))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			if err := copyInputDir(filepath.Join(src, "frames", e.Name()), filepath.Join(dst, "frames", e.Name())); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{"manifest.json", "audio.pcm"} {
		if err := copyFile(filepath.Join(src, name), filepath.Join(dst, name)); err != nil {
			return err
		}
	}
	return nil
}

func copyInputDir(src, dst string) error {
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() {
			if err := copyInputDir(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

func countCorrupt(g *gen.Result) int {
	n := 0
	for _, c := range g.Cameras {
		if c.Corrupt {
			n++
		}
	}
	return n
}

func countMissing(g *gen.Result) int {
	n := 0
	for _, c := range g.Cameras {
		if c.Missing {
			n++
		}
	}
	return n
}

func verifyCmd(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", "", "demo output directory")
	input := fs.String("input", "", "generated input directory (default DIR/input)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "fieldcut verify: --dir is required")
		return 2
	}
	inDir := *input
	if inDir == "" {
		inDir = filepath.Join(*dir, "input")
	}
	if _, err := os.Stat(inDir); err != nil {
		fmt.Fprintf(os.Stderr, "fieldcut verify: input %s: %v\n", inDir, err)
		return 2
	}
	v, err := runner.Verify(filepath.Join(*dir, "export"), inDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fieldcut verify: %v\n", err)
		return exitCode(err)
	}
	fmt.Printf("verify=PASS frames=%d root_sha256=%s sync_max_ms=%.1f recovery_rate=%.2f eligible=%d recovered=%d p95_frame_ms=%.0f audio_offset_ms=%.3f pulses=%d\n",
		v.Frames, v.RootOnDisk, v.SyncMaxMs, v.RecoveryRate, v.Eligible, v.Recovered, v.P95FrameMs, float64(v.AudioOffsetNs)/1e6, v.Pulses)
	return 0
}
