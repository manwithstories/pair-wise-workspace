// Command mediad generates self-contained media fixtures, runs the
// multi-track processing DAG and prints a reproducible terminal report.
//
// Usage:
//
//	mediad demo   --out ./tmp/demo [--corrupt] [--interrupt-after N]
//	mediad resume --out ./tmp/demo
//
// Exit codes: 0 complete success, 2 recoverable/incomplete result,
// 1 invalid arguments or an unrecoverable error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mediad/internal/pipeline"
	"mediad/internal/verify"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return verify.ExitFatal
	}
	switch args[0] {
	case "demo":
		return cmdDemo(args[1:])
	case "resume":
		return cmdResume(args[1:])
	case "-h", "--help", "help":
		usage()
		return verify.ExitFatal
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
		usage()
		return verify.ExitFatal
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mediad {demo|resume} --out DIR [--corrupt] [--interrupt-after N]")
}

type commonFlags struct {
	out            string
	corrupt        bool
	interruptAfter int
}

func parseFlags(fs *flag.FlagSet, args []string) (*commonFlags, error) {
	c := &commonFlags{}
	fs.StringVar(&c.out, "out", "", "output directory for fixtures, checkpoints and report")
	fs.BoolVar(&c.corrupt, "corrupt", false, "inject recoverable corruptions into the fixtures")
	fs.IntVar(&c.interruptAfter, "interrupt-after", 0, "stop scheduling after N leaf nodes complete")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if c.out == "" {
		return nil, fmt.Errorf("--out is required")
	}
	return c, nil
}

func cmdDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	cfg, err := parseFlags(fs, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: %v\n", err)
		return verify.ExitFatal
	}
	root := filepath.Join(cfg.out, "fixtures")
	// --corrupt is opt-in; without it the demo generates the healthy tree so
	// a plain "demo" exits 0, matching the acceptance contract.
	if _, err := pipeline.Generate(root, !cfg.corrupt); err != nil {
		fmt.Fprintf(os.Stderr, "demo: generate fixtures: %v\n", err)
		return verify.ExitFatal
	}
	return execute(root, cfg.out, cfg.interruptAfter)
}

func cmdResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	cfg, err := parseFlags(fs, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resume: %v\n", err)
		return verify.ExitFatal
	}
	root := filepath.Join(cfg.out, "fixtures")
	if _, err := os.Stat(root); err != nil {
		fmt.Fprintf(os.Stderr, "resume: no prior run under %s: %v\n", cfg.out, err)
		return verify.ExitFatal
	}
	start := time.Now()
	code := executeTimed(root, cfg.out, 0, &start, true)
	return code
}

func execute(root, out string, interruptAfter int) int {
	start := time.Now()
	return executeTimed(root, out, interruptAfter, &start, false)
}

func executeTimed(root, out string, interruptAfter int, start *time.Time, isResume bool) int {
	eng, err := pipeline.NewEngine(root, out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "engine: %v\n", err)
		return verify.ExitFatal
	}
	res, err := eng.Run(interruptAfter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pipeline: %v\n", err)
		return verify.ExitFatal
	}
	elapsed := time.Since(*start).Milliseconds()
	res.Report.ElapsedMS = elapsed
	if isResume {
		res.Report.ResumeMS = elapsed
	}
	// Persist the timing-enriched report; sha256 is over stable fields only.
	reportPath := filepath.Join(out, "report.json")
	if err := res.Report.WriteTo(reportPath); err != nil {
		fmt.Fprintf(os.Stderr, "write report: %v\n", err)
		return verify.ExitFatal
	}
	printReport(res, isResume)
	return res.Exit
}

// printReport emits the contract fields as stable, human-checked JSON.
func printReport(res *pipeline.Outcome, isResume bool) {
	rep := res.Report
	view := map[string]any{
		"segments":         rep.Segments,
		"silence_ms":       rep.SilenceMS,
		"duplicates":       rep.Duplicates,
		"first_bad_offset": rep.FirstBadOffset,
		"blocked_node":     rep.BlockedNode,
		"resume_ms":        rep.ResumeMS,
		"elapsed_ms":       rep.ElapsedMS,
		"sha256":           rep.SHA256,
	}
	data, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode report: %v\n", err)
		return
	}
	label := "demo"
	if isResume {
		label = "resume"
	}
	fmt.Printf("mediad %s exit=%d\n%s\n", label, res.Exit, data)
}
