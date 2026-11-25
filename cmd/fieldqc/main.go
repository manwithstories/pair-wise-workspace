// Command fieldqc is the acceptance adapter for the on-set field QC
// pipeline. It synthesizes normal, damaged and recovered samples (no
// external media is read), runs them through manifest -> MP4/RTP
// analysis -> diagnosis -> bounded pipeline -> report, and prints one
// JSON line per case.
//
// Exit codes: 0 = samples clean / every sample handled as expected
// (--case all), 1 = invalid parameters or manifest, 2 = damaged input
// was located (a single damaged case or analyze/manifest finding),
// 3 = unrecoverable I/O error or cancellation.
package main

import (
	"fmt"
	"io"
	"os"
	"time"
)

const (
	exitOK = iota
	exitInvalid
	exitDamaged
	exitAborted
)

// allCases lists every demo scenario in deterministic order.
var allCases = []string{
	"normal",         // clean media, recoverable random loss
	"truncated-box",  // mdat cut short inside one track
	"edit-range",     // edit list entry beyond the media
	"rtp-burst",      // unrecoverable burst loss
	"audio-edge",     // cut point off the sample-block boundary
	"color-conflict", // BT.709 vs BT.2020 across tracks
}

type config struct {
	seed   int64
	hours  int
	tracks int
	loss   float64
	window time.Duration
}

type command struct {
	fn   func(argv []string) int
	help string
}

var commands = map[string]command{}

func registerCommand(name, help string, fn func([]string) int) {
	commands[name] = command{fn: fn, help: help}
}

func main() { os.Exit(run(os.Args[1:])) }

func run(argv []string) int {
	if len(argv) == 0 {
		printUsage(os.Stderr)
		return exitInvalid
	}
	switch argv[0] {
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		return exitOK
	}
	if c, ok := commands[argv[0]]; ok {
		return c.fn(argv[1:])
	}
	fmt.Fprintf(os.Stderr, "fieldqc: unknown command %q\n", argv[0])
	printUsage(os.Stderr)
	return exitInvalid
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `fieldqc - on-set multi-camera QC adapter

usage:
  fieldqc demo --case all|%s [--seed N] [--hours H] [--tracks T] [--loss F] [--window D]
  fieldqc manifest <path|->   validate a manifest JSON file (exit 1 if invalid)
  fieldqc cancel              demonstrate bounded-pipeline cancellation (exit 3)

exit codes:
  0  clean, or 'demo --case all' met every expectation
  1  invalid arguments / unknown case / invalid manifest
  2  damaged input located (a single damaged 'demo --case X')
  3  unrecoverable I/O error or cancellation (status "aborted")
`, joinCases())
}

func joinCases() string {
	out := allCases[0]
	for _, c := range allCases[1:] {
		out += "|" + c
	}
	return out
}
