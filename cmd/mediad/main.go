// Command mediad is the terminal adapter for the multi-camera media pipeline.
//
//	mediad fixtures --out DIR --seed N   generate self-contained sample media
//	mediad run --job JOB --work DIR ...  execute / resume the pipeline
//	mediad verify --work DIR --cases DIR validate the delivery
//
// Exit codes: 0 success, 2 argument error, 3 media rejection, 75 interrupt.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/fashionweek/mediad/internal/pipeline"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(pipeline.ExitArgs)
	}
	var err error
	var code int
	switch os.Args[1] {
	case "fixtures":
		err = runFixtures(os.Args[2:])
		code = pipeline.ExitArgs // replaced on success below
	case "run":
		code, err = runPipeline(os.Args[2:])
	case "verify":
		code, err = runVerify(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		os.Exit(pipeline.ExitOK)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(pipeline.ExitArgs)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		// Preserve a domain-specific code when the handler provided one.
		if code == pipeline.ExitArgs || code == 0 {
			os.Exit(pipeline.ExitArgs)
		}
		os.Exit(code)
	}
	if os.Args[1] == "fixtures" {
		os.Exit(pipeline.ExitOK)
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `mediad — multi-camera fMP4 HLS delivery pipeline

usage:
  mediad fixtures --out DIR --seed N
  mediad run --job JOB.json --work DIR [--interrupt-after N] [--resume]
  mediad verify --work DIR --cases DIR

exit codes: 0 ok, 2 argument error, 3 media rejection, 75 interrupt
`)
}

var _ = context.Background
