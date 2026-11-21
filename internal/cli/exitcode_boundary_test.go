package cli

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestExitCodesFromBinary also runs the built binary so the process exit
// status (not just the in-process ExitError) is covered.
func TestExitCodesFromBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "archivecheck")
	build := exec.Command("go", "build", "-o", bin, "archivecheck.local/archivecheck/cmd/archivecheck")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cases := []struct {
		args []string
		code int
	}{
		{[]string{"verify", "--fixture", "truncated", "--recover=false"}, 2},
		{[]string{"verify", "--fixture", "oversized", "--recover=false"}, 2},
		{[]string{"recover", "--fixture", "truncated", "--recover=true"}, 0},
		{[]string{"recover", "--fixture", "oversized", "--recover=true"}, 2},
		{[]string{"verify", "--fixture", "definitely-not-a-fixture"}, 3},
		{[]string{"demo", "--seed", "17", "--queue", "4", "--slow-consumer-ms", "2"}, 0},
	}
	for _, c := range cases {
		cmd := exec.Command(bin, c.args...)
		err := cmd.Run()
		code := 0
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if code != c.code {
			t.Fatalf("%v exit=%d want %d", c.args, code, c.code)
		}
	}
}
