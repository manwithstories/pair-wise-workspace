// Command archivectl recovers, remuxes and verifies local oral-history archives.
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
	var code int
	switch os.Args[1] {
	case "synth":
		code = runSynth(os.Args[2:])
	case "recover":
		code = runRecover(os.Args[2:])
	case "verify":
		code = runVerify(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		code = 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		code = 2
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: archivectl <synth|recover|verify> [flags]")
	fmt.Fprintln(os.Stderr, "  synth -out DIR -seed N")
	fmt.Fprintln(os.Stderr, "  recover -in FILE -out OUT.mp4 -manifest LIST.csv [-journal J.json] [-resume] [-target-kbps 1200] [-window 64]")
	fmt.Fprintln(os.Stderr, "  verify -source SRC.mp4 -output OUT.mp4 -manifest LIST.csv")
}
