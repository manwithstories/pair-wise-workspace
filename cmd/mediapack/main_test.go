package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCLIExitCodes(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code != 2 {
		t.Fatalf("no args code=%d, want 2", code)
	}

	out.Reset()
	errOut.Reset()
	code := run([]string{"verify", "--faults", "not-a-fault"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("bad fault code=%d, want 2; stderr=%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "unknown fault") {
		t.Fatalf("stderr=%q should explain bad fault", errOut.String())
	}
}
