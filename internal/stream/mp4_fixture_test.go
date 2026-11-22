package stream

import (
	"testing"

	"mediascan/internal/fixture"
)

// buildMP4 delegates to the shared fixture generator so tests and the demo
// exercise the same container bytes.
func buildMP4(t *testing.T, tracks []fixture.TrackSpec) []byte {
	t.Helper()
	data, err := fixture.MP4(tracks)
	if err != nil {
		t.Fatalf("fixture.MP4: %v", err)
	}
	return data
}
