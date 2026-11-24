package mp4

import "testing"

func TestGoMP4CrossCheckSequence(t *testing.T) {
	b, err := EncodeFragment(testFragment())
	if err != nil {
		t.Fatal(err)
	}
	seq, err := GoMP4Sequence(b)
	if err != nil {
		t.Fatalf("go-mp4 could not extract mfhd from generated fragment: %v", err)
	}
	if seq != 7 {
		t.Fatalf("go-mp4 sequence=%d want 7", seq)
	}
}
