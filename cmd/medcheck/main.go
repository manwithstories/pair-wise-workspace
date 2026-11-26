// Command medcheck inspects capture packets, access units, timecode,
// subtitles and GOP boundaries after a live multi-camera performance.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"medcheck/pipeline"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "medcheck: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: medcheck demo --case clean|damaged|recover --seed N [--strict] [--json] [--workdir DIR]")
}

func runDemo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	caseName := fs.String("case", "", "demo fixture case: clean, damaged, recover")
	seed := fs.Int64("seed", 17, "deterministic fixture seed")
	strict := fs.Bool("strict", false, "exit 3 when an unrecovered bad segment is detected")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON instead of text")
	workDir := fs.String("workdir", "out/medcheck", "atomic index and subtitle output directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if !pipeline.ValidCases[*caseName] {
		fmt.Fprintf(stderr, "medcheck: --case must be clean, damaged or recover, got %q\n", *caseName)
		return 2
	}

	rep, err := pipeline.Run(pipeline.Config{
		Case:    *caseName,
		Seed:    *seed,
		Strict:  *strict,
		JSON:    *asJSON,
		WorkDir: *workDir,
	})
	if err != nil {
		var ioErr *pipeline.IOError
		if errors.As(err, &ioErr) {
			fmt.Fprintf(stderr, "medcheck: %v\n", err)
			return 4
		}
		fmt.Fprintf(stderr, "medcheck: run failed: %v\n", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(stderr, "medcheck: encode report: %v\n", err)
			return 4
		}
	} else {
		fmt.Fprint(stdout, rep.String())
	}
	return rep.ExitCode()
}
