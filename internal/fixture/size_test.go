package fixture

import "testing"

// TestSEIBlobExceedsFaultOffset guarantees the 12-segment demo blob is
// large enough for the demo's 4096-byte truncation injection to land
// inside a real NAL unit rather than past the end of the data.
func TestSEIBlobExceedsFaultOffset(t *testing.T) {
	s := Generate(2601, 12, FaultSpec{})
	if got := len(s.Tracks[0].SEI); got <= 4096 {
		t.Fatalf("SEI blob size = %d, must exceed fault offset 4096", got)
	}
}
