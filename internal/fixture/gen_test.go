package fixture

import (
	"bytes"
	"testing"
)

// TestGenDeterministic verifies identical specs yield byte-identical
// files, which the acceptance summary's stable sha256 relies on.
func TestGenDeterministic(t *testing.T) {
	spec := []TrackSpec{
		{ID: 1, Kind: "video", Timescale: 90000, Samples: []SampleSpec{
			{Duration: 3000, Key: true}, {Duration: 3000},
		}},
		{ID: 2, Kind: "audio", Timescale: 48000, Samples: []SampleSpec{
			{Duration: 1600}, {Duration: 1600},
		}},
	}
	a, err := Gen(17, spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Gen(17, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("same seed must produce identical bytes")
	}
	c, err := Gen(18, spec)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, c) {
		t.Fatal("different seed should change payload")
	}
}

// TestStripMoov removes exactly the moov box.
func TestStripMoov(t *testing.T) {
	data, err := Gen(7, []TrackSpec{{ID: 1, Kind: "video", Timescale: 90000, Samples: []SampleSpec{{Duration: 3000, Key: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	stripped := StripMoov(data)
	if bytes.Contains(stripped, []byte("moov")) {
		t.Fatal("moov still present")
	}
	if len(stripped) >= len(data) {
		t.Fatal("stripped file must be smaller")
	}
}
