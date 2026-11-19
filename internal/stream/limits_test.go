package stream

import "testing"

func TestScanRejectsOversizedBitstream(t *testing.T) {
	if _, err := Scan(H264, make([]byte, MaxScanBytes+1)); err == nil {
		t.Fatal("oversized bitstream accepted")
	}
	if _, err := Scan(H264, nil); err == nil {
		t.Fatal("empty bitstream accepted")
	}
	if _, err := Scan(Codec("vp9"), []byte{1}); err == nil {
		t.Fatal("unsupported codec accepted")
	}
}
