// Command mediad is the terminal demo/verification front-end for the reusable
// media processing library. It performs no transcoding and needs no network.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"mediad/internal/core"
	"mediad/internal/fixture"
	"mediad/internal/manifest"
	"mediad/internal/pipeline"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stdout)
		return core.ExitFatal
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "resume":
		return runResume(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "-h", "--help", "help":
		usage(os.Stdout)
		return core.ExitComplete
	default:
		fmt.Fprintf(os.Stderr, "mediad: unknown subcommand %q\n", args[0])
		usage(os.Stderr)
		return core.ExitFatal
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "usage: mediad <demo|resume|verify> [flags]")
	fmt.Fprintln(w, "  demo   --case clean|damaged|cancel [--seed N] [--workers N] [--queue N] (--out DIR | --checkpoint DIR)")
	fmt.Fprintln(w, "  resume --checkpoint DIR")
	fmt.Fprintln(w, "  verify --manifest FILE --base DIR")
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	caseName := fs.String("case", "clean", "fixture case: clean|damaged|cancel")
	seed := fs.Int64("seed", 23, "deterministic seed")
	workers := fs.Int("workers", pipeline.DefaultConfig().Workers, "worker pool size")
	queue := fs.Int("queue", pipeline.DefaultConfig().Queue, "bounded queue length")
	out := fs.String("out", "", "output directory for clean/damaged manifests")
	checkpoint := fs.String("checkpoint", "", "checkpoint directory for the cancel case")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return core.ExitComplete
		}
		return core.ExitFatal
	}

	ctx, stop := signalContext()
	defer stop()

	switch *caseName {
	case "clean", "damaged":
		if *out == "" {
			fmt.Fprintln(os.Stderr, "error: --out is required for", *caseName)
			return core.ExitFatal
		}
		return runHealthyDemo(ctx, *caseName, *seed, *workers, *queue, *out)
	case "cancel":
		if *checkpoint == "" {
			fmt.Fprintln(os.Stderr, "error: --checkpoint is required for cancel")
			return core.ExitFatal
		}
		return runCancelDemo(ctx, *checkpoint)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown case %q\n", *caseName)
		return core.ExitFatal
	}
}

func runHealthyDemo(ctx context.Context, caseName string, seed int64, workers, queue int, outDir string) int {
	mediaRoot := filepath.Join(outDir, pipeline.MediaDir)
	var cfg fixture.Config
	if caseName == "damaged" {
		cfg = fixture.DamagedConfig(mediaRoot, seed)
	} else {
		cfg = fixture.CleanConfig(mediaRoot, seed)
	}
	if err := fixture.Generate(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error: generate fixtures: %v\n", err)
		return core.ExitCode(err)
	}

	rep, err := pipeline.Execute(ctx, pipeline.ExecuteParams{
		MediaRoot:    mediaRoot,
		ManifestPath: filepath.Join(outDir, "manifest.json"),
		Case:         caseName, Seed: seed,
		Config:        pipeline.Config{Workers: workers, Queue: queue},
		PathPrefix:    pipeline.MediaDir,
		VerifyBaseDir: outDir,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return core.ExitCode(err)
	}
	printHealthy(caseName, seed, rep)
	return rep.ExitCode
}

func runCancelDemo(ctx context.Context, dir string) int {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return core.ExitFatal
	}
	mediaRoot := filepath.Join(dir, pipeline.MediaDir)
	if err := fixture.Generate(fixture.CancelConfig(mediaRoot)); err != nil {
		fmt.Fprintf(os.Stderr, "error: generate cancel fixtures: %v\n", err)
		return core.ExitCode(err)
	}
	cfg := pipeline.Config{Workers: 1, Queue: 2}

	golden, err := pipeline.DryRun(ctx, mediaRoot, pipeline.MediaDir, dir, cfg)
	expected := ""
	if err == nil && golden != nil {
		expected = golden.ResultHash
	}

	rep, err := pipeline.Execute(ctx, pipeline.ExecuteParams{
		MediaRoot:      mediaRoot,
		CheckpointPath: filepath.Join(dir, pipeline.CheckpointName),
		Case:           "cancel", Seed: 23, Config: cfg,
		CancelAfterCommits: 3, ExpectedHash: expected,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return core.ExitCode(err)
	}
	fmt.Printf("case=cancel state=canceled committed=%d checkpoint=%s exit=%d\n",
		rep.Committed, dir, rep.ExitCode)
	return rep.ExitCode
}

func runResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	checkpoint := fs.String("checkpoint", "", "checkpoint directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return core.ExitComplete
		}
		return core.ExitFatal
	}
	if *checkpoint == "" {
		fmt.Fprintln(os.Stderr, "error: --checkpoint is required")
		return core.ExitFatal
	}
	ctx, stop := signalContext()
	defer stop()

	rep, err := pipeline.ResumeDir(ctx, *checkpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return core.ExitCode(err)
	}
	fmt.Printf("resumed=%d duplicate_emit=%d result_hash_equal=%t result_hash=%s exit=%d\n",
		rep.Resumed, rep.DuplicateEmit, rep.ResultHashEqual, rep.ResultHash, rep.ExitCode)
	return rep.ExitCode
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	manifestPath := fs.String("manifest", "", "path to manifest.json")
	base := fs.String("base", ".", "base directory for artifact paths")
	if err := fs.Parse(args); err != nil {
		return core.ExitFatal
	}
	if *manifestPath == "" {
		fmt.Fprintln(os.Stderr, "error: --manifest is required")
		return core.ExitFatal
	}
	b, err := os.ReadFile(*manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return core.ExitFatal
	}
	var mf manifest.Manifest
	if err := json.Unmarshal(b, &mf); err != nil {
		fmt.Fprintf(os.Stderr, "error: parse manifest: %v\n", err)
		return core.ExitFatal
	}
	vr := manifest.VerifyManifest(*base, &mf)
	fmt.Printf("sha256=%d/%d roundtrip=%d/%d missing=%d failed=%d\n",
		vr.SHAOK, vr.SHATotal, vr.RoundTripOK, vr.RoundTripTotal,
		len(vr.Missing), len(vr.SHAFailed)+len(vr.RoundTripFailed))
	if !vr.Complete() {
		return core.ExitFatal
	}
	if len(mf.Quarantined) > 0 {
		return core.ExitQuarantined
	}
	return core.ExitComplete
}

func printHealthy(caseName string, seed int64, rep *pipeline.Report) {
	st := rep.Manifest.Stats
	v := rep.Verify
	fmt.Printf("case=%s seed=%d fragments=%d quarantined=%d roundtrip=%d/%d keyframe_hit=%d/%d sha256=%d/%d representations=%d aligned=%d exit=%d\n",
		caseName, seed, st.Fragments, st.Quarantined,
		v.RoundTripOK, v.RoundTripTotal,
		st.KeyframeHits, st.KeyframeTotal,
		v.SHAOK, v.SHATotal,
		st.Representations, st.Aligned, rep.ExitCode)
}
