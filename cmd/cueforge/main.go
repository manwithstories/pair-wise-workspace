// cueforge merges multi-track timed metadata into a verifiable
// timeline, renders offscreen review frames and proves that a
// checkpoint-resumed run matches a complete run.
package main

import (
	"fmt"
	"os"
)

// Exit codes.
const (
	exitOK         = 0
	exitFailed     = 1
	exitCorrupt    = 2 // corrupt input, location info printed
	exitCheckpoint = 3 // checkpoint or local I/O failure
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: cueforge <command> [flags]")
		fmt.Fprintln(os.Stderr, "commands: demo")
		os.Exit(exitFailed)
	}
	var code int
	switch os.Args[1] {
	case "demo":
		code = runDemo(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		code = exitFailed
	}
	os.Exit(code)
}
