// Command mediad is the terminal entry point for the mediad processing
// library. It demonstrates the pipeline end to end and prints the acceptance
// fields; all real work lives in the internal packages.
package main

import (
	"fmt"
	"io"
	"os"
)

// Exit codes are part of the command's contract.
const (
	exitOK        = 0
	exitFailure   = 1
	exitInvalid   = 2
	exitRecovery  = 3
	exitThreshold = 4
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitInvalid
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "mediad: unknown command %q\n", args[0])
		usage(stderr)
		return exitInvalid
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `mediad - deterministic media timeline verifier

usage:
  mediad demo  [flags]     run the full pipeline and print acceptance fields
  mediad help              show this message

demo flags:
  --seed int               random seed for fixtures and payloads (default 17)
  --faults list            comma list of duplicate,reorder,burst-loss,corrupt-log
  --bandwidth-mbps list    comma list of bandwidth values in Mbps
  --interrupt-after stage  simulate a crash after this stage
  --resume                 reuse checksum-verified stages after a crash
  --dir path               working directory (default: temporary)
  --seeks int              index probes per track (default 11)
  --max-burst int          longest recoverable packet gap (default 8)
  --max-alignment-ms float alignment gate in milliseconds (default 1)
  --max-bitrate-delta-pct  bitrate deviation gate in percent (default 5)
  --min-psnr float         minimum quality gate in dB (default 30)
  --json                   print only the machine-readable summary

exit codes:
  0 verified   2 invalid input or metadata   3 recovery or commit failed
  4 quantitative gate failed
`)
}
