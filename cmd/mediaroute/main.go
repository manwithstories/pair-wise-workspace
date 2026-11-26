// Command mediaroute repairs container timing, rebuilds random-access
// indexes and re-chunks multi-camera MP4/MOV captures for fast delivery.
// It is a pure terminal tool: no GUI, no browser, no external services.
//
// Usage:
//
//	mediaroute demo [--seed N] [--minutes N] [--cameras N] [--bitrates N]
//	                [--queue-mib N] [--slow-consumer-ms N]
//	                [--strict-boundary] [--workdir DIR] [--keep]
//
// Exit codes: 0 = verification completed (isolated faults included),
// 1 = I/O or internal error, 2 = invalid manifest or unrecoverable
// media, 3 = unquarantined boundary deviation in strict mode.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "demo":
		os.Exit(runDemo(os.Args[2:]))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `mediaroute - multi-camera capture repair and chunked delivery

Commands:
  demo    generate fixtures, run the repair/index/chunk pipeline, verify output

Exit codes: 0 ok (faults quarantined), 1 I/O or internal error,
            2 invalid manifest or unrecoverable media,
            3 boundary deviation in strict mode`)
}
