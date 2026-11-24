// Command mediascan exercises the media preparation pipeline end to end.
//
// Usage:
//
//	mediascan demo   --seed 41 --drop-every 97 --interrupt-after index --resume
//	mediascan verify --seed 41
//
// Exit codes: 0 all invariants hold, 1 processing or verification failed,
// 2 invalid arguments.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"mediascan/internal/pipeline"
	"mediascan/internal/probe"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mediascan <demo|verify> [flags]")
		return 2
	}
	cmd := args[0]
	switch cmd {
	case "demo":
		return runDemo(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "-h", "--help", "help":
		fmt.Println("usage: mediascan <demo|verify> [flags]")
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		return 2
	}
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	seed := fs.Int64("seed", 41, "fixture seed")
	dropEvery := fs.Int("drop-every", 0, "drop every Nth RTP packet (0 disables)")
	interruptAfter := fs.String("interrupt-after", "", "stop after this stage (manifest|repair|probe|index)")
	resume := fs.Bool("resume", false, "resume from the checkpoint")
	damaged := fs.Bool("damaged", false, "include damaged segments, packets and metadata")
	truncate := fs.Int("truncate-tail", 0, "truncate this many bytes from the generated container")
	dataDir := fs.String("data-dir", "", "working directory for cache and checkpoints")
	reject := fs.Bool("reject", false, "fail on metadata conflicts instead of normalising")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *seed <= 0 || *dropEvery < 0 || *truncate < 0 {
		fmt.Fprintln(os.Stderr, "invalid argument: seed must be positive and drop-every non-negative")
		return 2
	}
	if *interruptAfter != "" && !isStage(*interruptAfter) {
		fmt.Fprintf(os.Stderr, "invalid --interrupt-after %q\n", *interruptAfter)
		return 2
	}
	pol := probe.DefaultPolicy()
	if *reject {
		pol = probe.Policy{Channels: probe.Reject, SampleRate: probe.Reject, Color: probe.Reject}
	}
	dir, cleanup, err := workDir(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer cleanup()

	sum, err := pipeline.Run(pipeline.Options{
		Seed:           *seed,
		DropEvery:      *dropEvery,
		Damaged:        *damaged,
		TruncateTail:   *truncate,
		InterruptAfter: *interruptAfter,
		Resume:         *resume,
		CacheDir:       dir + "/cache",
		DataDir:        dir,
		Policy:         pol,
	})
	if err != nil {
		if errors.Is(err, pipeline.ErrInterrupted) {
			fmt.Fprintf(os.Stderr, "interrupted after stage: %v\n", err)
			printSummary(sum)
			return 1
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	printSummary(sum)
	if !demoInvariantsHold(sum, *damaged) {
		fmt.Fprintln(os.Stderr, "invariant violation")
		return 1
	}
	return 0
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	seed := fs.Int64("seed", 41, "fixture seed")
	dataDir := fs.String("data-dir", "", "working directory for cache and checkpoints")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *seed <= 0 {
		fmt.Fprintln(os.Stderr, "invalid argument: seed must be positive")
		return 2
	}
	dir, cleanup, err := workDir(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer cleanup()

	sum, err := pipeline.Run(pipeline.Options{
		Seed:     *seed,
		CacheDir: dir + "/cache",
		DataDir:  dir,
		Resume:   true,
		Policy:   probe.DefaultPolicy(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Printf("SOURCE_MATCH=%t\n", sum.SourceMatch)
	fmt.Printf("INDEX_VALID=%t\n", sum.IndexValid)
	fmt.Printf("SOURCE_SHA256=%s\n", sum.SourceSHA256)
	fmt.Printf("OUTPUT_SHA256=%s\n", sum.OutputSHA256)
	if !sum.SourceMatch || !sum.IndexValid {
		fmt.Fprintln(os.Stderr, "verification failed")
		return 1
	}
	return 0
}

func isStage(s string) bool {
	for _, st := range pipeline.StageOrder {
		if s == st {
			return true
		}
	}
	return false
}

func printSummary(s pipeline.Summary) {
	fmt.Printf("AV_SYNC_MS=%d\n", s.AVSyncMs)
	fmt.Printf("LOSS_RECOVERY_PCT=%.1f\n", s.LossRecoveryPct)
	fmt.Printf("P95_LATENCY_MS=%d\n", s.P95LatencyMs)
	fmt.Printf("INDEX_POINTS=%d\n", s.IndexPoints)
	fmt.Printf("CACHE_HITS=%d\n", s.CacheHits)
	fmt.Printf("ERROR_SEGMENTS=%d\n", s.ErrorSegments)
	fmt.Printf("SOURCE_SHA256=%s\n", s.SourceSHA256)
	fmt.Printf("OUTPUT_SHA256=%s\n", s.OutputSHA256)
}

// demoInvariantsHold encodes the acceptance thresholds for the normal sample.
func demoInvariantsHold(s pipeline.Summary, damaged bool) bool {
	if s.AVSyncMs > 20 {
		return false
	}
	if s.P95LatencyMs > 500 {
		return false
	}
	if s.IndexPoints <= 0 || !s.IndexValid {
		return false
	}
	if len(s.SourceSHA256) != 64 || len(s.OutputSHA256) != 64 {
		return false
	}
	if damaged {
		return s.ErrorSegments > 0
	}
	return s.LossRecoveryPct >= 95
}

// workDir resolves the scratch directory and a cleanup function.
func workDir(explicit string) (string, func(), error) {
	if explicit != "" {
		if err := os.MkdirAll(explicit, 0o755); err != nil {
			return "", nil, err
		}
		return explicit, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "mediascan-*")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}
