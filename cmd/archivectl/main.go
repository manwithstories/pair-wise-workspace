// archivectl is the archive pipeline acceptance CLI (pure terminal).
package main

import "os"

func main() {
	os.Exit(realMain(os.Args))
}

func realMain(args []string) int {
	// Subcommands are wired up in demo.go (added with the pipeline).
	return 0
}
