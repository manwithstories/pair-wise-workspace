package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

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

// cmdRun, cmdVerify and cmdFixtures are wired to their implementations in
// later batches; until then they report that the feature is unavailable.
func cmdRun(ctx context.Context, args []string) error {
	return media.Errorf(media.KindInternal, media.Origin{}, "run", "not implemented yet")
}

func cmdVerify(ctx context.Context, args []string) error {
	return media.Errorf(media.KindInternal, media.Origin{}, "verify", "not implemented yet")
}

func cmdFixtures(ctx context.Context, args []string) error {
	return media.Errorf(media.KindInternal, media.Origin{}, "fixtures", "not implemented yet")
}
