package main

import (
	"fmt"
	"os"
)

func runRecover(args []string) int {
	fmt.Fprintln(os.Stderr, "recover command is being assembled")
	return 2
}
