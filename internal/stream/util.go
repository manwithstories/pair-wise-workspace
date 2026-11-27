package stream

import (
	"fmt"
	"os/exec"
	"strings"
)

// execOutput runs a command and returns stdout, trimming stderr into errors.
func execOutput(bin string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %v: %s", bin, err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
