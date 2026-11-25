// mediaqa is a terminal-only media QA tool: it scans MP4 sample tables,
// repairs timestamps, builds random-access indexes, remuxes valid
// samples, recovers RTP packet loss, and reports chain-wide metrics.
//
// Exit codes:
//
//	0  success (recoverable corruption included)
//	2  invalid manifest or usage
//	3  unrecoverable media corruption
//	4  quantitative metric out of bounds
package main

import (
	"flag"
	"fmt"
	"os"

	"mediaqa/internal/manifest"
	"mediaqa/internal/media"
	"mediaqa/internal/pipeline"
)

const (
	exitOK            = 0
	exitBadManifest   = 2
	exitUnrecoverable = 3
	exitMetrics       = 4
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitBadManifest
	}
	switch args[0] {
	case "acceptance":
		return acceptanceCmd(args[1:])
	case "run":
		return runCmd(args[1:])
	default:
		usage()
		return exitBadManifest
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  mediaqa acceptance --work DIR --seed N   self-generate assets and run the acceptance demo
  mediaqa run --work DIR --manifest FILE   process a JSONL manifest`)
}

// runCmd processes a user-supplied manifest inside a work directory.
func runCmd(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	work := fs.String("work", ".", "work directory (all paths are confined to it)")
	manifestPath := fs.String("manifest", "", "JSONL manifest path (inside the work directory)")
	if err := fs.Parse(args); err != nil {
		return exitBadManifest
	}
	if *manifestPath == "" {
		fmt.Fprintln(os.Stderr, "run: --manifest is required")
		return exitBadManifest
	}
	f, err := os.Open(*manifestPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return exitBadManifest
	}
	defer f.Close()
	m, err := manifest.Parse(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return exitBadManifest
	}
	outDir := *work + "/out"
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return exitUnrecoverable
	}
	res, err := pipeline.Run(m, pipeline.Config{WorkDir: *work, OutDir: outDir})
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return exitBadManifest
	}
	printErrors(&res.Errs)
	if res.Errs.HasFatal() {
		return exitUnrecoverable
	}
	return exitOK
}

func printErrors(errs *media.List) {
	for _, e := range errs.All() {
		fmt.Fprintf(os.Stderr, "  %s\n", e)
	}
}
