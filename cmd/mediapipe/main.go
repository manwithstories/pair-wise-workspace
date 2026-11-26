package main

import (
	"flag"
	"fmt"
	"os"

	"mediapipe/internal/manifest"
	"mediapipe/internal/media"
	"mediapipe/internal/pipeline"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(4)
	}
	var err error
	switch os.Args[1] {
	case "demo":
		err = runDemo(os.Args[2:])
	case "verify-fixture":
		err = runVerifyFixture(os.Args[2:])
	default:
		usage()
		os.Exit(4)
	}
	if err != nil {
		code := exitCode(err)
		me, _ := media.AsError(err)
		line := map[string]any{"event": "error", "error_code": codeName(err), "exit_code": code, "error": err.Error()}
		if me != nil && me.Code != "" {
			line["error_code"] = string(me.Code)
		}
		_ = manifest.JSONLine(os.Stderr, "error", line)
		os.Exit(code)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mediapipe demo --work DIR [--seed N] [--seconds N] [--target-kbps N] [--fault-suite]")
	fmt.Fprintln(os.Stderr, "       mediapipe verify-fixture --case truncated_moov|timestamp_rollback|unrecoverable_loss|digest_tampered|checkpoint_corrupt|io_error --work DIR")
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	work := fs.String("work", ".tmp/demo", "working directory")
	seed := fs.Int64("seed", 1, "deterministic seed")
	seconds := fs.Int("seconds", 30, "generated media duration in seconds")
	targetKbps := fs.Int("target-kbps", 1200, "target total bitrate")
	faultSuite := fs.Bool("fault-suite", false, "run corruption/recovery fixtures")
	if err := fs.Parse(args); err != nil {
		return media.WrapError(media.CodeInvalidInput, "parse demo flags", err)
	}
	res, err := pipeline.RunDemo(pipeline.Options{Work: *work, Seed: *seed, Seconds: *seconds, TargetKbps: *targetKbps})
	if err != nil {
		return err
	}
	stats := res.Manifest.Stats
	stats.Case = "normal"
	if err := manifest.JSONLine(os.Stdout, "result", stats); err != nil {
		return err
	}
	if res.Manifest.Recovery != nil {
		if err := manifest.JSONLine(os.Stdout, "recovery", res.Manifest.Recovery); err != nil {
			return err
		}
	}
	if *faultSuite {
		lines := pipeline.RunFaultSuite(*work, func(l manifest.FaultLine) {
			_ = manifest.JSONLine(os.Stdout, "fault", l)
		})
		for _, l := range lines {
			if !l.OK {
				return media.NewError(media.CodeInvalidInput, "fault fixture did not produce expected rejection: "+l.Case, nil)
			}
		}
	}
	return nil
}

func runVerifyFixture(args []string) error {
	fs := flag.NewFlagSet("verify-fixture", flag.ContinueOnError)
	work := fs.String("work", ".tmp/fixture", "fixture working directory")
	caseName := fs.String("case", "", "fault case")
	if err := fs.Parse(args); err != nil {
		return media.WrapError(media.CodeInvalidInput, "parse verify flags", err)
	}
	wanted := *caseName
	if wanted == "checkpoint_error" {
		wanted = "checkpoint_corrupt"
	}
	lines := pipeline.RunFaultSuite(*work, nil)
	for _, l := range lines {
		if l.Case == wanted || l.ErrorCode == wanted {
			_ = manifest.JSONLine(os.Stdout, "fault", l)
			if !l.OK {
				return media.NewError(media.CodeInvalidInput, "fixture did not reject as expected", nil)
			}
			// Replay the mapped operational exit code while keeping the JSON record.
			return codedNil(l.ExitCode)
		}
	}
	return media.NewError(media.CodeInvalidInput, "unknown fixture case: "+*caseName, nil)
}

type codedExit struct{ code int }

func (c codedExit) Error() string { return fmt.Sprintf("exit %d", c.code) }
func (c codedExit) ExitCode() int { return c.code }
func codedNil(code int) error {
	if code == 0 {
		return nil
	}
	return codedExit{code: code}
}

func exitCode(err error) int {
	if c, ok := err.(interface{ ExitCode() int }); ok {
		return c.ExitCode()
	}
	if me, ok := media.AsError(err); ok {
		return me.ExitCode()
	}
	return 1
}

func codeName(err error) string {
	if me, ok := media.AsError(err); ok {
		return string(me.Code)
	}
	return "error"
}
