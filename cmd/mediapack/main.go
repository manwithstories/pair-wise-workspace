// Command mediapack is the terminal entry point for the streaming
// Matroska/WebM segmenting pipeline. It generates self-contained demo
// material, verifies manifest checksums and exercises crash recovery.
//
// Exit codes:
//
//	0 - success
//	1 - bad arguments or runtime error
//	2 - corrupt input, with the byte offset reported
//	3 - checkpoint or checksum mismatch
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediapack/internal/checkpoint"
	"mediapack/internal/clock"
	"mediapack/internal/fixture"
)

const (
	exitOK        = 0
	exitRuntime   = 1
	exitCorrupt   = 2
	exitMismatch  = 3
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitRuntime
	}
	switch args[0] {
	case "demo":
		return cmdDemo(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	case "recover":
		return cmdRecover(args[1:])
	case "-h", "--help", "help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage()
		return exitRuntime
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mediapack - streaming Matroska/WebM segmenting pipeline

usage:
  mediapack demo --generate --out DIR [--queue-bytes N] [--watermark-ms N]
  mediapack verify --dir DIR
  mediapack recover --dir DIR [--interrupt-after N]

exit codes: 0 ok, 1 runtime/args, 2 corrupt input (offset reported),
             3 checkpoint/checksum mismatch
`)
}

// exitFor maps a pipeline error onto the required process exit code.
func exitFor(err error) int {
	if err == nil {
		return exitOK
	}
	if me, ok := clock.AsMediaError(err); ok {
		switch me.Category {
		case clock.CatTruncated, clock.CatBadBlock, clock.CatBadVINT,
			clock.CatBadPath, clock.CatOversized, clock.CatOverflow:
			return exitCorrupt
		case clock.CatCheckpoint, clock.CatAlignment:
			return exitMismatch
		}
	}
	if _, ok := err.(*checkpoint.Mismatch); ok {
		return exitMismatch
	}
	return exitRuntime
}

func printJSON(v any) {
	raw, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(raw))
}

func loadFixtureData(name string) ([]byte, error) {
	all := fixture.All()
	f, ok := all[name]
	if !ok {
		return nil, fmt.Errorf("unknown fixture %q", name)
	}
	return f.Data, nil
}

func writeSource(dir string, data []byte) (string, error) {
	path := filepath.Join(dir, "source.webm")
	return path, os.WriteFile(path, data, 0o644)
}

func mustOutput(t string, err error) int {
	fmt.Fprintf(os.Stderr, "%s: %v\n", t, err)
	return exitFor(err)
}
