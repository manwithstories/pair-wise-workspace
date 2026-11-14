package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"m3u8ctl/internal/fixture"
	"m3u8ctl/internal/gen"
	"m3u8ctl/internal/verify"
)

func main() {
	os.Exit(execute(os.Args[1:]))
}

func execute(args []string) (code int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			fmt.Fprintf(os.Stderr, "m3u8ctl: internal error: %v\n", recovered)
			code = 3
		}
	}()
	if len(args) == 0 {
		printUsage()
		return 2
	}
	switch args[0] {
	case "fixture":
		return runFixture(args[1:])
	case "gen":
		return runGen(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "m3u8ctl: unknown subcommand %q\n", args[0])
		printUsage()
		return 2
	}
}

func runFixture(args []string) int {
	flags := flag.NewFlagSet("fixture", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	out := flags.String("out", "", "fixture output directory")
	scenario := flags.String("scenario", "clean", "clean|gap-reorder|dup|truncated|empty|pcr-rollback")
	renditions := flags.Int("renditions", 3, "number of renditions (1-3)")
	segments := flags.Int("segments", 30, "segments per rendition")
	seed := flags.Int64("seed", 0, "deterministic payload seed")
	if err := flags.Parse(args); err != nil {
		return flagExit(err)
	}
	if *out == "" {
		return usageError("fixture: --out is required")
	}
	err := fixture.Generate(fixture.Options{
		Out: *out, Scenario: *scenario, Renditions: *renditions, Segments: *segments, Seed: *seed,
	})
	if err != nil {
		return commandError("fixture", err)
	}
	fmt.Fprintf(os.Stdout, "fixture: wrote %s/live (%s, %d renditions x %d segments)\n", *out, *scenario, *renditions, *segments)
	return 0
}

func runGen(args []string) int {
	flags := flag.NewFlagSet("gen", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	input := flags.String("input", "", "live segment input directory")
	out := flags.String("out", "", "playlist output directory")
	window := flags.Int("window", 0, "retain only the newest N segments (0 = all)")
	targetDuration := flags.Float64("target-dur", 2, "target segment duration in seconds")
	endList := flags.Bool("endlist", false, "append EXT-X-ENDLIST")
	concurrency := flags.Int("concurrency", 4, "parallel probes")
	if err := flags.Parse(args); err != nil {
		return flagExit(err)
	}
	if *input == "" || *out == "" {
		return usageError("gen: --input and --out are required")
	}
	if *targetDuration <= 0 {
		return usageError("gen: --target-dur must be positive")
	}
	result, err := gen.Generate(gen.Config{
		Input: *input, Output: *out, Window: *window,
		TargetDuration: time.Duration(*targetDuration * float64(time.Second)),
		EndList:        *endList, Concurrency: *concurrency,
	})
	if err != nil {
		return commandError("gen", err)
	}
	fmt.Fprintf(os.Stdout, "gen: wrote %s (%d variants)\n", result.Master, len(result.Variants))
	return 0
}

func runVerify(args []string) int {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	root := flags.String("root", "", "playlist root directory or master.m3u8")
	format := flags.String("format", "table", "table|json")
	toleranceMS := flags.Int64("tolerance-ms", 100, "EXTINF tolerance in milliseconds")
	out := flags.String("out", "", "write report to this file")
	concurrency := flags.Int("concurrency", 4, "parallel variant checks")
	if err := flags.Parse(args); err != nil {
		return flagExit(err)
	}
	if *root == "" {
		return usageError("verify: --root is required")
	}
	if *format != "table" && *format != "json" {
		return usageError("verify: --format must be table or json")
	}
	if *toleranceMS < 0 {
		return usageError("verify: --tolerance-ms must not be negative")
	}
	report, err := verify.Run(verify.Config{
		Root: *root, Tolerance: time.Duration(*toleranceMS) * time.Millisecond, Concurrency: *concurrency,
	})
	if err != nil {
		return commandError("verify", err)
	}
	var writer *os.File
	if *out == "" {
		writer = os.Stdout
	} else {
		writer, err = os.Create(*out)
		if err != nil {
			return commandError("verify", err)
		}
		defer writer.Close()
	}
	if *format == "json" {
		err = report.WriteJSON(writer)
	} else {
		err = report.WriteTable(writer)
	}
	if err != nil {
		return commandError("verify output", err)
	}
	if *out != "" {
		fmt.Fprintf(os.Stdout, "verify: wrote %s\n", *out)
	}
	if report.Failed() {
		return 1
	}
	return 0
}

func flagExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

func usageError(message string) int {
	fmt.Fprintln(os.Stderr, message)
	return 2
}

func commandError(name string, err error) int {
	fmt.Fprintf(os.Stderr, "m3u8ctl %s: %v\n", name, err)
	return 2
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `usage: m3u8ctl <fixture|gen|verify> [flags]

  fixture  generate synthetic TS segments and anomaly scenarios
  gen      probe segments and emit master plus media playlists
  verify   cross-check playlists, segments, durations and bandwidth`)
}
