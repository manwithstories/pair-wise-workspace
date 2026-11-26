package audio

import (
	"testing"

	"mediamix/internal/clock"
)

// TestSilenceRoundTrip verifies a digital silence fragment preserves exact
// zero content through resample and stereo up-mix (no injected DC offset).
func TestSilenceRoundTrip(t *testing.T) {
	c, _ := NewConverter(DefaultSource(), DefaultTarget())
	silent := make([]byte, 320*2)
	out, err := c.Convert(silent)
	if err != nil {
		t.Fatal(err)
	}
	tail, err := c.Flush()
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, tail...)
	for i, v := range out {
		if v != 0 {
			t.Fatalf("non-zero sample at %d: %v", i, v)
		}
	}
	if _, err := c.Flush(); err != nil {
		t.Fatalf("double flush should be a no-op without error, got %v", err)
	}
	if _, err := c.Convert(silent); !clock.IsCode(err, clock.ErrInvalidParam) {
		t.Fatal("converter after close must return a locatable error")
	}
}
