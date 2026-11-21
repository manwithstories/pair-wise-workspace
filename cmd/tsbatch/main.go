// tsbatch is the terminal entry point of the MPEG-TS batch QC tool. It
// only forwards arguments to the driver library and exits with its
// status code: 0 success with verifiable results, 1 I/O failure,
// 2 argument error, 3 unrecoverable stream error.
package main

import (
	"os"

	"tsbatch/internal/driver"
)

func main() {
	os.Exit(driver.Main(os.Args))
}
