// Command mediad is a thin terminal adapter over the pipeline. All media logic
// lives in the internal packages; this file only parses flags, prints stage
// output and maps errors to documented exit codes.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"pairflow/internal/audit"
	"pairflow/internal/media"
	"pairflow/internal/pipeline"
	"pairflow/internal/transport"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage())
		return 2
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	case "resume":
		return runResume(args[1:], stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage())
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s", args[0], usage())
		return 2
	}
}

func usage() string {
	return `mediad — pairflow terminal adapter

usage:
  mediad demo   --seed N --seconds N --faults none|mixed|bad-frame-crc --out DIR [--interrupt-after render]
  mediad resume --run DIR
  mediad verify --run DIR

exit codes: 0 ok, 2 input/structure error, 3 integrity failure rolled back,
            4 resumable simulated interruption
`
}

func runDemo(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Uint64("seed", 17, "fixture seed")
	seconds := fs.Int("seconds", 3, "timeline length in seconds")
	faults := fs.String("faults", "none", "fault profile: none, mixed, bad-frame-crc")
	out := fs.String("out", "", "run directory (required)")
	interrupt := fs.String("interrupt-after", "", "stop after a stage (render)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" {
		fmt.Fprintf(stderr, "demo requires --out DIR\n%s", usage())
		return 2
	}
	switch *faults {
	case transport.ModeNone, transport.ModeMixed, transport.ModeBadFrame:
	default:
		fmt.Fprintf(stderr, "unknown --faults %q (none, mixed, bad-frame-crc)\n", *faults)
		return 2
	}
	switch strings.ToLower(*interrupt) {
	case "", "render", "compose":
	default:
		fmt.Fprintf(stderr, "unknown --interrupt-after %q (render)\n", *interrupt)
		return 2
	}

	_, err := pipeline.Run(pipeline.Options{
		RunDir:         *out,
		Seed:           *seed,
		Seconds:        *seconds,
		Faults:         *faults,
		InterruptAfter: *interrupt,
		Out:            stdout,
	})
	if err != nil {
		return reportError(stdout, stderr, *out, err)
	}
	fmt.Fprintf(stdout, "state=committed run=%s\n", *out)
	fmt.Fprint(stdout, hintVerify(*out))
	return 0
}

func runResume(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	runDir := fs.String("run", "", "run directory (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *runDir == "" {
		fmt.Fprintf(stderr, "resume requires --run DIR\n%s", usage())
		return 2
	}
	if _, err := pipeline.Resume(*runDir, stdout); err != nil {
		return reportError(stdout, stderr, *runDir, err)
	}
	fmt.Fprintf(stdout, "state=committed run=%s\n", *runDir)
	fmt.Fprint(stdout, hintVerify(*runDir))
	return 0
}

func runVerify(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	runDir := fs.String("run", "", "run directory (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *runDir == "" {
		fmt.Fprintf(stderr, "verify requires --run DIR\n%s", usage())
		return 2
	}
	res, err := audit.Verify(*runDir, nil)
	if err != nil {
		return reportError(stdout, stderr, *runDir, err)
	}
	fmt.Fprintf(stdout, "%s\n", res.Describe())
	for _, issue := range res.SortedIssues() {
		fmt.Fprintf(stdout, "issue=%s\n", issue)
	}
	if !res.FrameHashesOK || !res.StageArtifactsOK || len(res.Issues) > 0 {
		fmt.Fprintf(stdout, "verify=failed run=%s\n", *runDir)
		return 3
	}
	fmt.Fprintf(stdout, "verify=ok run=%s\n", *runDir)
	return 0
}

// reportError prints a locatable message and a state line, then returns the
// documented exit code for the error class.
func reportError(stdout, stderr *os.File, runDir string, err error) int {
	var me *media.Error
	code := 1
	state := "error"
	stage := ""
	if errors.As(err, &me) {
		code = me.Class.ExitCode()
		stage = me.Stage
		switch me.Class {
		case media.ClassIntegrity:
			state = "rolled_back"
		case media.ClassInterrupted:
			state = "interrupted"
		case media.ClassInput:
			state = "invalid"
		default:
			state = "failed"
		}
	}
	fmt.Fprintf(stderr, "error=%s\n", err.Error())
	if stage == "" {
		fmt.Fprintf(stdout, "state=%s run=%s committed=0\n", state, runDir)
	} else {
		fmt.Fprintf(stdout, "state=%s stage=%s run=%s committed=0\n", state, stage, runDir)
	}
	return code
}

func hintVerify(runDir string) string {
	return fmt.Sprintf("hint=run `mediad verify --run %s` for independent review\n", runDir)
}
