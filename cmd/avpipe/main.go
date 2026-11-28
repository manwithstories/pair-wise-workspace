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
	"strings"
	"syscall"
	"time"

	"avpipe/internal/media"
)

func main() {
	os.Exit(realMain(os.Args[1:]))
}

// realMain dispatches subcommands and converts their outcome into the
// documented exit codes.
func realMain(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return media.ExitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch args[0] {
	case "selfcheck":
		err = cmdSelfcheck(ctx, args[1:])
	case "verify":
		err = cmdVerify(ctx, args[1:])
	case "run":
		err = cmdRun(ctx, args[1:])
	case "fixtures":
		err = cmdFixtures(ctx, args[1:])
	case "-h", "--help", "help":
		usage(os.Stdout)
		return media.ExitOK
	default:
		fmt.Fprintf(os.Stderr, "avpipe: unknown command %q\n", args[0])
		usage(os.Stderr)
		return media.ExitUsage
	}
	if err != nil {
		// A canceled context surfaces as an internal error from the stage; map
		// it to the cancel exit code so operators get a stable signal.
		if errors.Is(err, context.Canceled) {
			err = media.New(media.KindCancel, media.Origin{}, "avpipe", "canceled", err)
		}
		fmt.Fprintf(os.Stderr, "avpipe: %v\n", err)
		return media.ExitCodeFor(err)
	}
	return media.ExitOK
}

func usage(w *os.File) {
	fmt.Fprint(w, `avpipe - local lossless MXF/MP4 segment pipeline

Usage:
  avpipe selfcheck --dir DIR [--seed N] [--workers N] [--queue N] [--segment D]
  avpipe verify --manifest FILE [--output FILE]
  avpipe run --input FILE [--input FILE] --output FILE --manifest FILE [options]
  avpipe fixtures --dir DIR [--seed N]

Options for run:
  --format mp4|mxf       output container (default mp4)
  --wrap-bits N          timestamp wrap width for DTS/PTS (default 33)
  --quarantine           quarantine missing timestamps / DTS regressions
  --allow-unmatched      allow inputs to contribute only mapped tracks
  --cancel-after N        cancel after N packets (tests cancellation)
  --cache N              reorder cache size (default = queue)

Exit codes:
  0 ok   1 internal   2 usage   3 corrupt/unsupported   4 canceled
`)
}

// cmdFixtures generates the fixture catalog under --dir.
func cmdFixtures(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fixtures", flag.ContinueOnError)
	dir := fs.String("dir", "", "fixture output directory (required)")
	seed := fs.Int64("seed", 17, "deterministic seed")
	if err := fs.Parse(args); err != nil {
		return media.New(media.KindUsage, media.Origin{}, "fixtures", err.Error(), err)
	}
	if *dir == "" {
		return media.Errorf(media.KindUsage, media.Origin{}, "fixtures", "--dir is required")
	}
	set, err := generateFixtures(*dir, *seed)
	if err != nil {
		return err
	}
	for _, f := range set.Files {
		fmt.Printf("fixture name=%s size=%d sha256=%s\n", filepath.Base(f.Path), f.Size, f.SHA256[:16])
	}
	return nil
}

// cmdVerify re-checks a manifest, optionally against its output file.
func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	manifest := fs.String("manifest", "", "manifest JSON (required)")
	output := fs.String("output", "", "output container to re-verify (optional)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return media.New(media.KindUsage, media.Origin{}, "verify", err.Error(), err)
	}
	if *manifest == "" {
		return media.Errorf(media.KindUsage, media.Origin{}, "verify", "--manifest is required")
	}
	target := *output
	if target == "" {
		// The manifest lives next to the output by convention; derive the
		// sibling good.mp4 when present so verify is self-sufficient.
		guess := filepath.Join(filepath.Dir(*manifest), "good.mp4")
		if _, err := os.Stat(guess); err == nil {
			target = guess
		}
	}
	v, err := verifyManifest(*manifest, target)
	if err != nil {
		return err
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	fmt.Printf("verify manifest=%s segments=%d hash_ok=%d hash_bad=%d output_ok=%d output_bad=%d digest_ok=%t\n",
		*manifest, v.Segments, v.HashOK, v.HashBad, v.OutputsOK, v.OutputsBad, v.OutputDigestOK)
	for _, p := range v.Problems {
		fmt.Printf("problem: %s\n", p)
	}
	if v.HashBad > 0 || v.OutputsBad > 0 || !v.OutputDigestOK {
		return media.Errorf(media.KindCorrupt, media.Origin{File: *manifest}, "verify",
			"manifest verification failed")
	}
	return nil
}

// cmdRun performs one conversion from the terminal.
func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var inputs stringList
	fs.Var(&inputs, "input", "input MXF/MP4 (repeatable)")
	output := fs.String("output", "", "output container (required)")
	manifest := fs.String("manifest", "", "manifest output (required)")
	format := fs.String("format", "mp4", "output container format")
	segment := fs.String("segment", "2s", "segment duration")
	workers := fs.Int("workers", 4, "worker count")
	queue := fs.Int("queue", 8, "queue size")
	cache := fs.Int("cache", 0, "reorder cache size (default = queue)")
	wrapBits := fs.Int("wrap-bits", 33, "timestamp wrap width")
	quarantine := fs.Bool("quarantine", false, "quarantine anomalous packets instead of failing")
	unmatched := fs.Bool("allow-unmatched", false, "allow unmapped tracks")
	cancelAfter := fs.Int64("cancel-after", 0, "cancel after N packets")
	if err := fs.Parse(args); err != nil {
		return media.New(media.KindUsage, media.Origin{}, "run", err.Error(), err)
	}
	seg, err := media.ParseDuration(*segment)
	if err != nil {
		return media.New(media.KindUsage, media.Origin{}, "run", "invalid --segment", err)
	}
	cacheN := *cache
	if cacheN <= 0 {
		cacheN = *queue
	}
	res, err := run(ctx, runConfig{
		Inputs: inputs, Output: *output, Manifest: *manifest, Format: *format,
		Segment: seg, Workers: *workers, Queue: *queue, Cache: cacheN,
		WrapBits: *wrapBits, AllowQuarantine: *quarantine,
		AllowUnmatchedTracks: *unmatched, CancelAfter: *cancelAfter,
	})
	if err != nil {
		return err
	}
	fmt.Printf("run output=%s manifest=%s segments=%d packets=%d bytes=%d tracks=%s\n",
		res.Output, res.ManifestPath, res.Segments, res.Packets, res.Bytes, strings.Join(res.TrackIDs, ","))
	return nil
}

// stringList collects a repeatable flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// nowUTC is swapped in tests for a fixed timestamp.
var nowUTC = func() time.Time { return time.Now().UTC() }
