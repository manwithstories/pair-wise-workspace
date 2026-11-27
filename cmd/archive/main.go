// Package archive is the CLI adapter: it wires the pipeline stages together and
// owns exit-code policy.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"archive/internal/fixture"
)

// Exit codes mandated by the acceptance contract.
const (
	ExitOK          = 0
	ExitUnrecover   = 2
	ExitDigest      = 3
	ExitInterrupted = 75
)

func main() {
	root := &cobra.Command{
		Use:           "archive",
		Short:         "Recover CMAF/fMP4 archives into a shared-clock timeline",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(fixtureCmd(), recoverCmd(), verifyCmd())
	root.SetOutput(os.Stdout)
	if err := root.Execute(); err != nil {
		code := ExitUnrecover
		var ce *exitCodeError
		if errors.As(err, &ce) {
			code = ce.Code
			if ce.Err == nil {
				os.Exit(code)
			}
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(code)
	}
}

// exitCodeError lets a command request a specific process exit status without
// losing the error text.
type exitCodeError struct {
	Code int
	Err  error
}

func (e *exitCodeError) Error() string {
	if e.Err == nil {
		return ""
	}
	return e.Err.Error()
}
func (e *exitCodeError) Unwrap() error { return e.Err }

func withExit(code int, err error) error {
	if code == ExitOK {
		return err
	}
	return &exitCodeError{Code: code, Err: err}
}

func fixtureCmd() *cobra.Command {
	opts := fixture.BuildOptions{}
	cmd := &cobra.Command{
		Use:   "fixture",
		Short: "Generate a self-contained recovery fixture",
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.OutDir == "" {
				return withExit(ExitUnrecover, errors.New("--out is required"))
			}
			res, err := fixture.Build(opts)
			if err != nil {
				return withExit(ExitUnrecover, err)
			}
			fmt.Printf("fixture profile=%s out=%s levels=%d faults=%d manifest=%s\n",
				res.Manifest.Profile, res.Manifest.Dir, len(res.Manifest.Levels), len(res.Manifest.Faults), res.ManifestPath)
			for _, l := range res.Manifest.Levels {
				fmt.Printf("  level=%s segments=%d bytes=%d sha256=%s\n", l.Level, l.SegmentCount, l.SizeBytes, l.SHA256)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.OutDir, "out", "", "output directory for the fixture")
	cmd.Flags().StringVar((*string)(&opts.Profile), "profile", "clean", "fixture profile: clean|mixed")
	cmd.Flags().IntVar(&opts.DurationSec, "duration", 8, "source duration in seconds")
	cmd.Flags().StringVar(&opts.FFmpeg, "ffmpeg", "", "ffmpeg binary override")
	cmd.Flags().BoolVar(&opts.ForceVFR, "vfr", false, "force variable frame rate on every level")
	return cmd
}

func recoverCmd() *cobra.Command {
	var (
		input       string
		out         string
		ckpt        string
		resume      bool
		interruptAt int
		thresholdUS int64
		noDecode    bool
		verbose     bool
	)
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Recover representations and fan them into a shared-clock timeline",
		RunE: func(cmd *cobra.Command, args []string) error {
			rc := runRecover(runOptions{
				Input: input, Out: out, Checkpoint: ckpt, Resume: resume,
				InterruptAfter: interruptAt, BoundaryUS: thresholdUS,
				NoDecode: noDecode, Verbose: verbose,
			})
			if rc.Err != nil {
				return withExit(rc.Code, rc.Err)
			}
			if rc.Code == ExitInterrupted {
				// Intentional interruption: no error text, exit 75.
				return withExit(rc.Code, nil)
			}
			if rc.Code != 0 {
				return withExit(rc.Code, errors.New("recovery incomplete"))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&input, "input", "", "fixture manifest.json")
	cmd.Flags().StringVar(&out, "out", "", "output directory")
	cmd.Flags().StringVar(&ckpt, "checkpoint", "", "checkpoint file path")
	cmd.Flags().BoolVar(&resume, "resume", false, "resume from the checkpoint")
	cmd.Flags().IntVar(&interruptAt, "interrupt-after", 0, "simulate interruption after N representations (exit 75)")
	cmd.Flags().Int64Var(&thresholdUS, "boundary-threshold-us", 2000, "max allowed representation boundary delta")
	cmd.Flags().BoolVar(&noDecode, "no-decode", false, "skip FFmpeg frame export (timeline only)")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "print per-representation recovery decisions")
	_ = context.Background()
	return cmd
}

func verifyCmd() *cobra.Command {
	var (
		input     string
		out       string
		threshold int64
	)
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify an exported recovery against its fixture contract",
		RunE: func(cmd *cobra.Command, args []string) error {
			rc := runVerify(input, out, threshold)
			if rc.Err != nil {
				return withExit(rc.Code, rc.Err)
			}
			if rc.Code != 0 {
				return withExit(rc.Code, errors.New("verification failed"))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&input, "input", "", "fixture manifest.json")
	cmd.Flags().StringVar(&out, "out", "", "recovery output directory to verify")
	cmd.Flags().Int64Var(&threshold, "boundary-threshold-us", 2000, "max allowed boundary delta")
	return cmd
}
