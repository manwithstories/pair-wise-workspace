// Command mediarun is the terminal entry point for the broadcast delivery
// pipeline. It parses arguments, runs the demo workflow, prints key=value
// results and exits with the documented status code.
package main

import (
	"fmt"
	"os"

	"mediarun/internal/cli"
	"mediarun/internal/mediaerr"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	args, err := cli.Parse(argv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "status=error")
		fmt.Fprintln(os.Stderr, "error_code="+codeOf(err))
		fmt.Fprintln(os.Stderr, "detail="+err.Error())
		fmt.Fprintln(os.Stderr, "output_committed=false")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, cli.Usage())
		return cli.ExitCode(err)
	}

	lines, _, err := cli.RunDemo(args)
	for _, line := range lines {
		fmt.Println(line)
	}
	if err != nil {
		return cli.ExitCode(err)
	}
	return 0
}

// codeOf extracts the stable machine-readable code from a structured error,
// falling back to a generic IO code for non-structured failures.
func codeOf(err error) string {
	if se, ok := err.(*mediaerr.Error); ok {
		return string(se.Code)
	}
	return string(mediaerr.CodeIO)
}
