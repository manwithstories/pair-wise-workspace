// Command mediaflow is the terminal-only entry point for the broadcast media
// verification and remux pipeline. All functionality is driven by flags and
// stdout; there is no graphical or network dependency.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"mediaflow/internal/pipeline"
	"mediaflow/internal/report"
)

// Exit codes per the acceptance contract.
const (
	exitOK         = 0
	exitQuality    = 1
	exitArgs       = 2
	exitIOOrCancel = 3
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return exitArgs
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "-h", "--help", "help":
		printUsage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		printUsage()
		return exitArgs
	}
}

type demoFlags struct {
	seed  int64
	root  string
	cases []string
}

func parseDemoFlags(args []string) (demoFlags, error) {
	f := demoFlags{seed: 41, root: "", cases: []string{
		pipeline.CaseClean, pipeline.CaseDamage, pipeline.CaseResume,
	}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("flag %s requires a value", a)
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--seed":
			v, err := next()
			if err != nil {
				return f, err
			}
			if _, err := fmt.Sscanf(v, "%d", &f.seed); err != nil {
				return f, fmt.Errorf("invalid --seed %q: %w", v, err)
			}
		case a == "--root":
			v, err := next()
			if err != nil {
				return f, err
			}
			if v == "" {
				return f, fmt.Errorf("--root must not be empty")
			}
			f.root = v
		case a == "--cases":
			v, err := next()
			if err != nil {
				return f, err
			}
			f.cases = strings.Split(v, ",")
		default:
			return f, fmt.Errorf("unknown flag %q", a)
		}
	}
	if f.root == "" {
		return f, fmt.Errorf("--root is required")
	}
	for _, c := range f.cases {
		switch c {
		case pipeline.CaseClean, pipeline.CaseDamage, pipeline.CaseResume:
		default:
			return f, fmt.Errorf("unknown case %q (want clean, damage or resume)", c)
		}
	}
	return f, nil
}

func runDemo(args []string) int {
	flags, err := parseDemoFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "argument error: %v\n", err)
		return exitArgs
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	th := report.DefaultThresholds()
	var allMetrics []report.Metrics
	qualityFailed := false

	for _, name := range flags.cases {
		caseRoot := flags.root + "/" + name
		out, err := pipeline.Run(ctx, name, pipeline.Config{
			Seed: flags.seed, Root: caseRoot, ReorderWindow: 16,
		})
		if err != nil {
			// Cancellation and I/O failures map to exit code 3.
			fmt.Fprintf(os.Stderr, "case %s failed: %v\n", name, err)
			return exitIOOrCancel
		}
		// The damage case is allowed unrecovered gaps; clean/resume must not.
		allowGaps := name == pipeline.CaseDamage
		gate := out.Metrics.Evaluate(th, allowGaps)
		if !gate.Pass {
			qualityFailed = true
			fmt.Fprintf(os.Stderr, "case %s gate failures: %s\n", name, strings.Join(gate.Failures, ","))
		}
		if name == pipeline.CaseDamage && len(out.Gaps) == 0 {
			qualityFailed = true
			fmt.Fprintf(os.Stderr, "case %s expected unrecovered gaps but none reported\n", name)
		}
		allMetrics = append(allMetrics, out.Metrics)
	}

	report.SortByCase(allMetrics)
	fmt.Println(report.Header())
	for _, m := range allMetrics {
		fmt.Println(m.Row())
	}
	if qualityFailed {
		return exitQuality
	}
	return exitOK
}

func printUsage() {
	fmt.Fprint(os.Stderr, `mediaflow - broadcast media verification and remux

usage:
  mediaflow demo --seed <int> --root <dir> --cases <clean,damage,resume>

exit codes:
  0  metrics passed
  1  quality gate failed
  2  argument error
  3  I/O error or cancellation
`)
}
