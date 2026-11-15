package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"rcctl/internal/plan"
	"rcctl/internal/state"
)

const (
	exitOK        = 0
	exitRuntime   = 1
	exitThreshold = 2
	exitInvalid   = 3
	exitJournal   = 4
)

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

func runCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return exitInvalid
	}
	switch args[0] {
	case "plan":
		return commandPlan(args[1:], stdout, stderr)
	case "run":
		return commandRun(args[1:], stdout, stderr, false)
	case "resume":
		return commandRun(args[1:], stdout, stderr, true)
	case "verify":
		return commandVerify(args[1:], stdout, stderr)
	case "report":
		return commandReport(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "rcctl: unknown command %q\n", args[0])
		printUsage(stderr)
		return exitInvalid
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `rcctl - offline rate-control and quality regression tool

usage:
  rcctl plan   --clip news --frames 240 --gop 20 --target-kbps 1200 --out work/plan.json
  rcctl run    --plan work/plan.json --workers 3 --journal work/journal.log
  rcctl resume --plan work/plan.json --journal work/journal.log
  rcctl verify --report work/report.json --max-dev-pct 5 --max-psnr-std 1.5
  rcctl report --plan work/plan.json --journal work/journal.log --out work/report.json

exit codes: 0 pass, 1 runtime error, 2 threshold failure, 3 invalid input, 4 journal/plan mismatch`)
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) int {
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitInvalid
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(fs.Output(), "%s: unexpected arguments: %v\n", fs.Name(), fs.Args())
		return exitInvalid
	}
	return -1
}

func failInvalid(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "rcctl: %v\n", err)
	return exitInvalid
}

func resolveReportPath(reportPath, journalPath string) string {
	if reportPath != "" {
		return reportPath
	}
	return filepath.Join(filepath.Dir(journalPath), "report.json")
}

func validateJournal(snapshot *state.Snapshot, p *plan.Plan) error {
	expected := make([]state.ExpectedSegment, len(p.Segments))
	for i, seg := range p.Segments {
		expected[i] = state.ExpectedSegment{Frames: seg.Frames, TargetKbps: seg.TargetKbps}
	}
	return snapshot.ValidateSegments(p.PlanID, expected)
}
