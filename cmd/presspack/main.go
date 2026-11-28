// presspack is a local media delivery kernel for post-production handoff:
// it probes multi-camera recordings, recovers damaged inputs, transcodes to
// two-tier CMAF HLS, slices subtitles onto the segment grid, writes
// manifests and audits delivery quality. Terminal-only; no network use.
//
// Exit codes: 0 = all metrics pass, 1 = parameter/dependency/processing
// failure, 2 = partial delivery or metrics below threshold.
package main

import (
	"fmt"
	"os"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitPartial = 2
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitFailure
	}
	switch args[0] {
	case "demo":
		return cmdDemo(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	case "-h", "--help", "help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
		usage()
		return exitFailure
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `presspack - local multi-camera delivery kernel

usage:
  presspack demo   --seed N --seconds S --segment SEC --targets B1,B2 --out DIR
      Generate three camera fixtures (healthy, truncated, missing-index,
      cross-day timecode, truncated subtitles), run the pipeline twice and
      print acceptance metrics.
  presspack verify --dir DIR
      Re-audit a previous demo output, safely re-running the pipeline
      (completed variants are resumed, never re-segmented).

exit codes: 0 metrics pass, 1 failure, 2 partial delivery / below threshold
`)
}

// checkDeps verifies the external binaries the pipeline shells out to.
func checkDeps() error {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := lookPath(bin); err != nil {
			return fmt.Errorf("dependency %q not found in PATH: %w", bin, err)
		}
	}
	return nil
}
