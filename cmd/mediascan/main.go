// Command mediascan is the acceptance entry point for the media damage
// scanner. It works entirely on the local filesystem: the demo command
// synthesizes its own fixtures and the resume command reuses a previously
// written scan directory.
package main

import (
	"os"

	"mediascan/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
