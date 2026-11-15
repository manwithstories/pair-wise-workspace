package main

import (
	"flag"
	"fmt"
	"os"

	"relayd/internal/report"
)

func cmdReport(args []string) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	file := fs.String("file", "", "report JSON file to read back and validate")
	if err := fs.Parse(args); err != nil {
		return exitStats
	}
	if *file == "" {
		fmt.Fprintln(os.Stderr, "report: --file is required")
		return exitStats
	}
	r, err := report.ReadFile(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitStats
	}
	if err := report.Validate(r); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitStats
	}
	b, err := report.Encode(r)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitStats
	}
	fmt.Println(string(b))
	return exitOK
}
