package integration

import (
	"testing"

	"mediacheck/internal/probe"
)

func analyzeProbe(t *testing.T, path string) *probe.File {
	t.Helper()
	f, err := probe.Analyze(path, probe.DefaultConfig())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	return f
}
