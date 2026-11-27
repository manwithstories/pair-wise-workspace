// Package main is the relayctl acceptance adapter. The reusable media
// processing logic lives under internal/; this entry point only parses
// terminal arguments and renders machine-readable results.
package main

import "os"

// Exit codes shared by every subcommand. They are stable contract values:
//
//	0  completion
//	2  damaged input located
//	3  unrecoverable packet loss
//	4  invalid arguments
//	75 paused per parameters, awaiting resume
const (
	exitOK        = 0
	exitDamaged   = 2
	exitLost      = 3
	exitUsage     = 4
	exitPaused    = 75
)

func main() {
	code := run(osArgs())
	os.Exit(code)
}
