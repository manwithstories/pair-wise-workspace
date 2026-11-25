// Command mediaaudit is the terminal front-end for the multi-track media
// audit pipeline. It generates fixtures, runs/resumes/verifies scans and emits
// deterministic reports. All processing is local with no network access.
package main

import (
	"errors"
	"fmt"
	"os"

	"mediaaudit/internal/model"
)

// Exit codes form the stable CLI contract.
const (
	exitOK            = 0
	exitInvalid       = 2
	exitIndexBad      = 3
	exitInterrupted   = 75
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitInvalid)
	}
	var err error
	switch os.Args[1] {
	case "fixture":
		err = runFixture(os.Args[2:])
	case "scan":
		err = runScan(os.Args[2:], false)
	case "resume":
		err = runResume(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(exitInvalid)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mediaaudit:", err)
		os.Exit(exitCodeFor(err))
	}
}

func exitCodeFor(err error) int {
	switch {
	case errors.Is(err, model.ErrInterrupted):
		return exitInterrupted
	case errors.Is(err, model.ErrIndexUnrecoverable):
		return exitIndexBad
	case errors.Is(err, model.ErrInvalid):
		return exitInvalid
	default:
		return exitInvalid
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mediaaudit - local multi-track PCM/RTP/MP4 audit

usage:
  mediaaudit fixture --dir DIR --seed N --channels C --frames F --tile-frames T
                    [--damage-frame N] [--wrap-at N] [--drift-ppm F] [--vfr a,b,c]
  mediaaudit scan   --package DIR --index FILE --chunk-frames N
                    --checkpoint-every N [--interrupt-after-checkpoint N]
  mediaaudit resume  --package DIR --index FILE --report FILE
  mediaaudit verify  --package DIR --index FILE --report FILE

exit codes: 0 ok (located damage allowed), 2 invalid param/asset,
             3 index unrecoverable, 75 deliberate checkpoint interruption
`)
}
