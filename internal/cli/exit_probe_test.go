package cli

import (
	"io"
	"testing"
)

func TestExitCodeProbe(t *testing.T) {
	err := Run([]string{"verify", "--fixture", "truncated", "--recover=false"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected error")
	}
	if e, ok := err.(*ExitError); ok {
		t.Logf("ExitError code=%d", e.Code)
	} else {
		t.Logf("non-ExitError: %T %v", err, err)
	}
}
