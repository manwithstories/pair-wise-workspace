// Command mediacheck runs batch media quality verification and remuxing.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"mediacheck/internal/demo"
	"mediacheck/internal/media"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] != "verify" {
		fmt.Fprintln(os.Stderr, "usage: mediacheck verify --out DIR [--faults list] [--resume]")
		return 3
	}
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	out := fs.String("out", "", "output root; all artifacts remain below this directory")
	faultsFlag := fs.String("faults", "", "comma-separated injected faults")
	resume := fs.Bool("resume", false, "reuse hash-verified committed WAL stages")
	memoryMiB := fs.Int("memory-mib", 32, "Go runtime soft memory limit per job")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 3
	}
	if *memoryMiB > 0 {
		debug.SetMemoryLimit(int64(*memoryMiB) << 20)
	}

	faults := map[string]bool{}
	for _, f := range strings.Split(*faultsFlag, ",") {
		f = strings.TrimSpace(f)
		if f != "" {
			faults[f] = true
		}
	}
	err := demo.Run(demo.Config{Out: *out, Faults: faults, Resume: *resume})
	if err != nil {
		code := media.ExitCode(err)
		fmt.Fprintf(os.Stderr, "verify failed exit_code=%d: %v\n", code, err)
		return code
	}
	fmt.Println("result=all_expected_paths_verified exit_code=0")
	return 0
}
