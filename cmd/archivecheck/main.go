// Command archivecheck is the terminal acceptance entry point for the archive
// pipeline. It generates its own fixtures, runs a full demux/remux/verify demo,
// verifies arbitrary Matroska files, and performs recovery passes.
package main

import (
	"fmt"
	"os"

	"archivecheck.local/archivecheck/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		var code int
		if e, ok := err.(*cli.ExitError); ok {
			code = e.Code
			if e.Message != "" {
				fmt.Fprintln(os.Stderr, e.Message)
			}
		} else {
			code = 3
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(code)
	}
}
