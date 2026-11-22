// Command mediascan is a terminal-only media inspection and timeline tool.
// It generates small local fixtures, analyzes MP4 sample structure, keyframe
// intervals, RTP packet sequences and subtitles, segments a unified timeline
// with watermark/replay semantics, and prints per-segment details.
//
// Usage:
//
//	mediascan demo [flags]
//
// Flags (demo):
//
//	--seed N           fixture seed (default 41)
//	--late-event N     deliver N events after the watermark (default 0)
//	--truncate-mp4 N   cut N bytes off the MP4 fixture's tail
//	--drop-index N     omit N keyframe index entries (rebuild path)
//	--corrupt-samples N mark N samples with a corrupt declared length
//	--subtitle-format  srt|vtt (default srt)
//
// Exit codes: 0 = summary and consistency checks passed; 2 = localized but
// recoverable input faults; 1 = processing or consistency failure.
package main

import (
	"fmt"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 1
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "-h", "--help", "help":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "mediascan: unknown command %q\n", args[0])
		usage(os.Stderr)
		return 1
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "usage: mediascan demo [--seed N] [--late-event N] [--truncate-mp4 N] [--drop-index N] [--corrupt-samples N] [--subtitle-format srt|vtt]")
}
