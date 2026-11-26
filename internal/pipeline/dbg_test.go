package pipeline

import (
	"testing"

	"mediad/internal/timeline"
)

// TestPipelinePackage verifies the shared late window is the production value;
// behavioural coverage lives in the delivery tests.
func TestPipelinePackage(t *testing.T) {
	if timeline.LatenessWindow != 2*48000 {
		t.Fatalf("late window = %d", timeline.LatenessWindow)
	}
}
