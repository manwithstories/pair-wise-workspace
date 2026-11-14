// Command mp4replay probes, indexes, seeks, exports and frame-hashes MP4
// gameplay replays without any re-encoding.
package main

import (
	"fmt"
	"os"

	"mp4replay/internal/report"
)

// command handlers are implemented in commands.go and grow batch by batch.
type handler func(args []string) (int, error)

var commands = map[string]handler{}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(report.ExitArgs)
	}
	name := os.Args[1]
	fn, ok := commands[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown sub-command: %s\n", name)
		usage()
		os.Exit(report.ExitArgs)
	}
	code, err := fn(os.Args[2:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err.Error())
		var ce *report.Error
		if asReport(err, &ce) {
			os.Exit(ce.Code)
		}
		os.Exit(report.ExitBoxInvalid)
	}
	os.Exit(code)
}

func asReport(err error, target **report.Error) bool {
	for err != nil {
		if e, ok := err.(*report.Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func usage() {
	fmt.Fprintln(os.Stderr,
		"usage: mp4replay <probe|index|seek|export|hash|gen|verify> [flags]")
}
