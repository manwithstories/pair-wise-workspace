package main

import (
	"fmt"
	"io"

	reportpkg "pairwise.local/relayctl/internal/report"
)

func runReport(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("report", stderr)
	input := flags.String("input", "out/report.json", "report JSON path")
	format := flags.String("format", "table", "table or json")
	if !parseFlags(flags, args) {
		return exitConfig
	}
	result, err := reportpkg.Load(*input)
	if err != nil {
		fmt.Fprintf(stderr, "load report: %v\n", err)
		return exitConfig
	}
	switch *format {
	case "table":
		err = reportpkg.WriteTable(stdout, result)
	case "json":
		err = reportpkg.WriteLine(stdout, result)
	default:
		fmt.Fprintf(stderr, "unknown report format %q\n", *format)
		return exitConfig
	}
	if err != nil {
		fmt.Fprintf(stderr, "write report: %v\n", err)
		return exitConfig
	}
	return exitOK
}
