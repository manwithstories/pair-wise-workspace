package main

import (
	"os"
	"os/exec"
)

// osWrite writes a test fixture file, keeping os usage in one place.
func osWrite(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}

// exitCode extracts the process exit code from an exec error.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}
