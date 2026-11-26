// mediad is a terminal-only local media analysis kernel. Exit codes:
// 0 success (recoverable sections completed), 2 invalid manifest or
// parameters, 3 no decodable frames, 4 local I/O failure.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"mediad/internal/fixture"
	"mediad/internal/manifest"
	"mediad/internal/pipeline"
	"mediad/internal/report"
)

type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func main() {
	root := newRoot()
	root.SilenceErrors = true
	root.SilenceUsage = true
	if err := root.Execute(); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			fmt.Fprintln(os.Stderr, "error:", ee.msg)
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "mediad",
		Short: "local multi-track media analysis kernel",
	}
	var out string
	var seed, targetBPS int64
	var frames int
	demo := &cobra.Command{
		Use:   "demo",
		Short: "generate fixtures and run all cases",
		RunE: func(cmd *cobra.Command, args []string) error {
			return demoRun(out, seed, targetBPS, frames, cmd.OutOrStdout())
		},
	}
	demo.Flags().StringVar(&out, "out", "", "output directory (required)")
	demo.Flags().Int64Var(&seed, "seed", 1, "fixture random seed")
	demo.Flags().Int64Var(&targetBPS, "target-bps", 1800000, "proxy target bitrate")
	demo.Flags().IntVar(&frames, "frames", 120, "frames per success/corrupt case")
	_ = demo.MarkFlagRequired("out")

	var manifestDir string
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "run the pipeline on an existing manifest directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOnce(manifestDir, targetBPS, cmd.OutOrStdout())
		},
	}
	runCmd.Flags().StringVar(&manifestDir, "dir", "", "manifest directory (required)")
	runCmd.Flags().Int64Var(&targetBPS, "target-bps", 1800000, "proxy target bitrate")
	_ = runCmd.MarkFlagRequired("dir")

	root.AddCommand(demo, runCmd)
	return root
}

func loadManifest(dir string) (*manifest.Manifest, error) {
	f, err := os.Open(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return manifest.Decode(f)
}

// runCase executes one case directory and maps the outcome to exit codes.
func runCase(name, dir string, targetBPS int64) report.CaseReport {
	base := report.CaseReport{Case: name, TargetBPS: targetBPS}
	m, err := loadManifest(dir)
	if err != nil {
		base.ExitCode = 2
		return base
	}
	res, err := pipeline.Run(context.Background(), m, pipeline.Options{
		Dir: dir, TargetBPS: targetBPS})
	if err != nil {
		base.ExitCode = 4
		return base
	}
	code := 0
	if res.Frames == 0 {
		code = 3
	}
	return report.FromResult(name, targetBPS, res, code)
}

func demoRun(out string, seed, targetBPS int64, frames int, stdout io.Writer) error {
	if out == "" || targetBPS <= 0 || frames <= 0 || frames > 10000 {
		return &exitError{2, "invalid parameters"}
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return &exitError{4, err.Error()}
	}
	cases, err := fixture.GenerateAll(out, seed, frames)
	if err != nil {
		return &exitError{4, err.Error()}
	}
	rep := &report.Report{Version: 1}
	for _, c := range cases {
		cr := runCase(c.Name, c.Dir, targetBPS)
		rep.Cases = append(rep.Cases, cr)
		fmt.Fprintln(stdout, cr.Line())
	}
	f, err := os.Create(filepath.Join(out, "report.json"))
	if err != nil {
		return &exitError{4, err.Error()}
	}
	defer f.Close()
	if err := report.Write(f, rep); err != nil {
		return &exitError{4, err.Error()}
	}
	for _, cr := range rep.Cases {
		if cr.Case == "success" && cr.ExitCode != 0 {
			return &exitError{cr.ExitCode, "success case failed"}
		}
	}
	return nil
}

func runOnce(dir string, targetBPS int64, stdout io.Writer) error {
	if dir == "" || targetBPS <= 0 {
		return &exitError{2, "invalid parameters"}
	}
	cr := runCase(filepath.Base(dir), dir, targetBPS)
	fmt.Fprintln(stdout, cr.Line())
	if cr.ExitCode != 0 {
		return &exitError{cr.ExitCode, "run failed"}
	}
	return nil
}
