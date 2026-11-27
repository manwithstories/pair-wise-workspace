package main

import (
	"fmt"
	"os"
)

// osArgs is indirected so tests can invoke the dispatcher deterministically.
func osArgs() []string { return os.Args }

// run dispatches a single relayctl invocation and returns the process exit
// code. Subcommand implementations are filled in by later modules.
func run(args []string) int {
	if len(args) < 2 {
		return usageErr("missing subcommand: fixture|run|resume|verify|inspect")
	}
	switch args[1] {
	case "fixture":
		return cmdFixture(args[2:])
	case "run":
		return cmdRun(args[2:])
	case "resume":
		return cmdResume(args[2:])
	case "verify":
		return cmdVerify(args[2:])
	case "inspect":
		return cmdInspect(args[2:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, helpText)
		return exitOK
	default:
		return usageErr("unknown subcommand %q", args[1])
	}
}

func usageErr(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "relayctl: "+format+"\n", a...)
	return exitUsage
}

const helpText = `relayctl - regional live relay recovery tool

usage:
  relayctl fixture  --case <name> --seed <n> --out <dir>
  relayctl run      --input <dir> --state <dir> [--pause-after <node>]
  relayctl resume   --input <dir> --state <dir>
  relayctl verify   --input <dir> --state <dir> --repeat <n>
  relayctl inspect  --input <dir>

exit codes: 0 done, 2 damaged input, 3 unrecoverable loss,
            4 bad arguments, 75 paused awaiting resume
`
