package pipeline_test

import (
	"testing"

	"mediapack/internal/pipeline"
)

func TestSafetyLimitsAreSane(t *testing.T) {
	if pipeline.MaxInputBytes <= 0 || pipeline.MaxFragmentBytes <= 0 {
		t.Fatal("limits must be positive")
	}
	if pipeline.MaxFragmentBytes > pipeline.MaxInputBytes {
		t.Fatal("a fragment cannot be allowed to exceed the whole input")
	}
	if pipeline.MaxPathComponents <= 0 {
		t.Fatal("path component limit must be positive")
	}
}

func TestErrInterruptedMessage(t *testing.T) {
	err := &pipeline.ErrInterrupted{Fragments: 8}
	if err.Error() == "" {
		t.Fatal("interruption error must carry a message")
	}
}
