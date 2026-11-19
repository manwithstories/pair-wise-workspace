// Command mediaroute recovers damaged RTP/RTCP live-stream captures,
// unifies track timelines, writes a random-accessible fragmented MP4
// with a provenance manifest, and supports checkpoint/resume.
//
// Exit codes:
//
//	0  success
//	2  invalid arguments or input (limits exceeded, digest mismatch)
//	3  media boundary corruption (dependency cycle, unmappable time, bad index)
//	10 interrupted on request; checkpoint saved
package main

import (
	"fmt"
	"os"
)

const (
	exitOK        = 0
	exitUsage     = 2
	exitMedia     = 3
	exitInterrupt = 10
)

func usage() {
	fmt.Fprintf(os.Stderr, `mediaroute - live-stream RTP recovery and MP4 indexing

usage:
  mediaroute demo    --seed N --out DIR
  mediaroute run     --input F --checkpoint F --out F [--interrupt-after N]
                     [--window N] [--drift-ppm N]
  mediaroute verify  --dir DIR
  mediaroute query   --manifest F [--track N] [--from-ns N] [--to-ns N]
`)
	os.Exit(exitUsage)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	code := exitOK
	switch os.Args[1] {
	case "demo":
		code, err = cmdDemo(os.Args[2:])
	case "run":
		code, err = cmdRun(os.Args[2:])
	case "verify":
		code, err = cmdVerify(os.Args[2:])
	case "query":
		code, err = cmdQuery(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediaroute: %v\n", err)
	}
	if code != exitOK {
		os.Exit(code)
	}
}
