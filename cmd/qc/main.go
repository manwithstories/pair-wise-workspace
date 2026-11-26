package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"mediaqc/internal/media"
	"mediaqc/internal/pipeline"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "demo" {
		fmt.Fprintln(stderr, "usage: qc demo --profile <normal|burst|corrupt> [--codec opus|aac] [--seed N] [--fail-once NODE]")
		return 1
	}

	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := fs.String("profile", "normal", "network profile")
	codec := fs.String("codec", "opus", "audio codec")
	seed := fs.Int64("seed", 41, "deterministic seed")
	failOnce := fs.String("fail-once", "", "DAG node whose first attempt fails once")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}

	out, err := pipeline.Run(pipeline.Config{
		Profile:  *profile,
		Codec:    *codec,
		Seed:     *seed,
		FailOnce: *failOnce,
	})
	if out == nil {
		kind := media.Classify(err)
		fmt.Fprintf(stdout, "error_type=%s\n", kind)
		fmt.Fprintf(stderr, "error [%s]: %v\n", kind, err)
		return media.ExitCode(err)
	}

	printDAGEvents(stdout, out)
	out.Stats.PrintKeyValues(stdout)

	if err != nil {
		kind := media.Classify(err)
		if out.Stats.Malformed > 0 {
			fmt.Fprintf(stdout, "malformed=%d\n", out.Stats.Malformed)
			if len(out.Malformed) > 0 {
				fmt.Fprintf(stdout, "malformed_detail=%s\n", out.Malformed[0])
			}
		}
		if out.Stats.Conflicts > 0 {
			fmt.Fprintf(stdout, "conflicts=%d\n", out.Stats.Conflicts)
		}
		fmt.Fprintf(stdout, "error_type=%s\n", kind)
		fmt.Fprintf(stderr, "error [%s]: %v\n", kind, err)
		return media.ExitCode(err)
	}

	for name, ok := range out.Stats.Thresholds() {
		if !ok {
			fmt.Fprintf(stdout, "threshold_failed=%s\n", name)
			return 1
		}
	}
	fmt.Fprintln(stdout, "status=ok")
	return 0
}

func printDAGEvents(w io.Writer, out *pipeline.Output) {
	if out.DAG == nil {
		return
	}
	for _, e := range out.DAG.Events {
		switch e.Kind {
		case "attempt_failed":
			fmt.Fprintf(w, "dag node=%s attempt=%d result=failed error_type=%s\n", e.NodeID, e.Attempt, e.ErrorType)
		case "blocked_dependents":
			fmt.Fprintf(w, "dag blocked_dependency=%s blocked_nodes=%v\n", e.NodeID, e.Blocked)
		case "retry_success":
			fmt.Fprintf(w, "dag node=%s attempt=%d result=retry_success output_sha256=%s\n", e.NodeID, e.Attempt, e.OutputHash)
		}
	}
}
