package main

import (
	"flag"
	"fmt"
	"os"

	"mediqc/internal/media"
	"mediqc/internal/ts"
)

func runInspect(args []string) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	input := fs.String("input", "", "path to a bare 188/204-byte MPEG-TS file")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *input == "" {
		fmt.Fprintln(os.Stderr, "inspect: --input is required")
		return 1
	}
	b, err := os.ReadFile(*input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "inspect: %v\n", err)
		return 1
	}
	report, merr := ts.DetectSync(b, media.DefaultLimits())
	fmt.Printf("input=%s bytes=%d\n", *input, len(b))
	fmt.Printf("first_desync_offset=%d packet_size=%d resync_offset=%d checked_packets=%d\n",
		report.FirstDesync, report.PacketSize, report.ResyncOffset, report.Checked)
	if merr != nil {
		printMediaError("inspect", merr)
		if merr.Fatal {
			return exitCodeFor(merr)
		}
	}
	return 0
}
