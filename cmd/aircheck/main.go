// Command aircheck verifies multirate delivery packages and packages them
// into HLS for broadcast and archive. Subcommands: fixture, scan, run, audit.
// Exit codes: 0 pass, 1 material/quality failure, 2 runtime error.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"aircheck/internal/fault"
)

func main() { os.Exit(dispatch(os.Args[1:])) }

func dispatch(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	var err error
	switch args[0] {
	case "fixture":
		err = cmdFixture(args[1:])
	case "scan":
		err = cmdScan(args[1:])
	case "run":
		err = cmdRun(args[1:])
	case "audit":
		err = cmdAudit(args[1:])
	default:
		usage()
		return 2
	}
	return exitErr(err)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  aircheck fixture --seed N --out DIR
  aircheck scan    --input DIR --cache DIR
  aircheck run     --input DIR --cache DIR --out DIR [--resume]
  aircheck audit   --input DIR --output DIR [--json]
exit codes: 0 pass, 1 material/quality failure, 2 runtime error`)
}

// exitErr maps faults to exit 1 (JSON on stdout) and everything else to 2.
func exitErr(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, errGates) {
		return 1 // report already printed
	}
	if fe := fault.Of(err); fe != nil {
		raw, _ := json.MarshalIndent(fe, "", "  ")
		fmt.Println(string(raw))
		return 1
	}
	fmt.Fprintln(os.Stderr, "runtime error:", err)
	return 2
}

func printJSON(v any) {
	raw, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(raw))
}
