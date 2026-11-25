// Command mediad runs the regional media distribution demo.
package main

import (
	"fmt"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "help", "-h", "--help":
		printUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		printUsage(os.Stderr)
		return 2
	}
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, "usage: mediad demo --out DIR [--codec all|h264|h265|av1]")
	fmt.Fprintln(w, "       [--seed N] [--target-bps N] [--drift-ppm N]")
	fmt.Fprintln(w, "       [--inject wrap,duplicate,truncate,no-key] [--resume]")
}
