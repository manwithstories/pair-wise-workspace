// Command segctl slices MPEG-TS captures into HLS playlists and verifies
// rendition consistency as a release gate.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const usage = `segctl - marathon replay slicing & verification toolchain

usage: segctl <command> [flags]

commands:
  fixture   synthesize four-rendition TS captures with fault injection
  slice     reorder, de-duplicate and re-slice one or all renditions
  manifest  generate master and media playlists from sliced segments
  verify    check playlist/disk consistency and emit the gate report

exit codes: 0 pass, 2 gaps only (degraded release), 3 blocking, 1 runtime error
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	var code int
	var err error
	switch os.Args[1] {
	case "fixture":
		err = runFixture(os.Args[2:])
	case "slice":
		err = runSlice(os.Args[2:])
	case "manifest":
		code, err = runManifest(os.Args[2:])
	case "verify":
		code, err = runVerify(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "segctl: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "segctl:", err)
		if code != 0 {
			os.Exit(code)
		}
		os.Exit(1)
	}
	os.Exit(code)
}

// writeFileAtomic writes data via a temp file + rename so an interrupted
// run never leaves a half-written artifact behind.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
