package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"archive.local/archive/internal/pipeline"
)

// Exit codes are part of the contract.
const (
	exitOK      = 0
	exitFailed  = 1
	exitDamaged = 2
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitFailed)
	}
	switch os.Args[1] {
	case "demo":
		os.Exit(runDemo(os.Args[2:]))
	case "verify":
		os.Exit(runVerify(os.Args[2:]))
	case "help", "-h", "--help":
		usage()
		os.Exit(exitOK)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(exitFailed)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `archive — local film-archive repair and indexing tool

  archive demo   [flags]   generate fixtures, repair, index and report
  archive verify [flags]   re-verify a previous run's index digest

demo flags:
  --seed N           deterministic seed (default 41)
  --duration D       material length, e.g. 3600s (default 1h)
  --corrupt-at N     byte offset to damage in the video source
  --interrupt-after N  packets to confirm before simulating an interruption
  --resume           resume from the checkpoint left by the interrupted run
  --workdir PATH     explicit working directory (default: fresh temp dir)
  --json             emit the summary as JSON as well as text`)
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	seed := fs.Int64("seed", 41, "deterministic seed")
	duration := fs.Duration("duration", time.Hour, "material duration")
	corruptAt := fs.Int64("corrupt-at", 184320, "byte offset to damage the source mdat")
	interruptAfter := fs.Int("interrupt-after", 1200, "packets confirmed before interruption")
	resume := fs.Bool("resume", false, "resume from the checkpoint")
	workdir := fs.String("workdir", "", "explicit working directory")
	asJSON := fs.Bool("json", false, "also emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitFailed
	}
	work, cleanup, err := ensureWorkdir(*workdir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workdir: %v\n", err)
		return exitFailed
	}
	defer cleanup()

	// Fixtures are generated whenever they are absent, so the documented
	// one-shot command works in a fresh directory. A repeat invocation with
	// the same working directory resumes from the checkpoint instead.
	needFixtures := !fixtureSetPresent(work)
	if needFixtures {
		if err := prepareFixtures(work, *seed, (*duration).Truncate(time.Second), *corruptAt); err != nil {
			fmt.Fprintf(os.Stderr, "fixtures: %v\n", err)
			return exitFailed
		}
	}

	// With --resume in a fresh directory there is nothing to resume yet, so
	// first record what an interrupted transfer would have left behind.
	hasCheckpoint := checkpointExists(work)
	if *resume && !hasCheckpoint {
		if err := simulateInterrupt(work, opts(work, *duration), *interruptAfter, *seed); err != nil {
			fmt.Fprintf(os.Stderr, "interrupt: %v\n", err)
			return exitFailed
		}
		hasCheckpoint = true
	}

	runOpts := opts(work, *duration)
	runOpts.Resume = *resume && hasCheckpoint
	res, err := pipeline.Run(runOpts)
	if err != nil {
		if res == nil || res.Status == pipeline.Failed {
			fmt.Fprintf(os.Stderr, "run failed: %v\n", err)
			if res != nil {
				printResult(res)
			}
			return exitFailed
		}
	}
	printResult(res)
	if res == nil {
		return exitFailed
	}
	if *asJSON {
		emitJSON(res)
	}
	return res.Status.ExitCode()
}

// ensureWorkdir creates a private working directory when none is given.
func ensureWorkdir(dir string) (string, func(), error) {
	if dir == "" {
		tmp, err := os.MkdirTemp("", "archive-demo-*")
		if err != nil {
			return "", func() {}, err
		}
		if err := os.Chmod(tmp, 0o700); err != nil {
			return "", func() {}, err
		}
		return tmp, func() { os.RemoveAll(tmp) }, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", func() {}, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", func() {}, err
	}
	return abs, func() {}, nil
}

func opts(work string, d time.Duration) pipeline.Options {
	return pipeline.Options{
		WorkDir:      work,
		ManifestPath: "material.txt",
		TracePath:    filepath.Join(work, "transport.trace"),
		Duration:     d,
	}
}

// prepareFixtures writes the MP4 material, the manifest and the RTP trace.
