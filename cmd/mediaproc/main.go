// Command mediaproc is the terminal adapter for deterministic media processing.
package main

import (
	"flag"
	"fmt"
	"os"

	"mediaproc/internal/app"
	"mediaproc/internal/model"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: mediaproc demo [flags]")
		os.Exit(1)
	}

	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 41, "deterministic fixture seed")
	out := fs.String("out", "", "output directory")
	maxMemory := fs.Int64("max-memory", 8<<20, "maximum accounted queue memory in bytes")
	queue := fs.Int("queue", 4, "bounded queue capacity")
	slowConsumer := fs.Duration("slow-consumer", 0, "simulated publish delay per segment")
	fault := fs.String("fault", "", "fault injection: truncated-block|invalid-vint|missing-cluster-end|queue-cancel")
	recoverFault := fs.Bool("recover", false, "recover at the next validated Cluster boundary")
	cancelAfter := fs.Duration("cancel-after", 0, "cancel a full queue after this delay")
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(1)
	}

	opts := app.Options{
		Seed:         *seed,
		Out:          *out,
		MaxMemory:    *maxMemory,
		Queue:        *queue,
		SlowConsumer: *slowConsumer,
		Fault:        *fault,
		Recover:      *recoverFault,
		CancelAfter:  *cancelAfter,
	}
	report, err := app.RunDemo(opts)
	if err != nil {
		if report != nil && report.Recoveries > 0 {
			fmt.Println(report.RecoverySummary())
		}
		fmt.Fprintf(os.Stderr, "mediaproc: %v\n", err)
		os.Exit(model.ExitCode(err))
	}

	if report.Recoveries > 0 {
		fmt.Println(report.RecoverySummary())
	} else {
		fmt.Println(report.NormalSummary())
	}
	if !report.MemoryOK() {
		fmt.Fprintf(os.Stderr, "memory invariant failed: peak=%d limit=%d\n",
			report.PeakMemory, report.MemoryLimit)
		os.Exit(2)
	}
	os.Exit(report.ExitCode)
}
