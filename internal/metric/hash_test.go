package metric

import "testing"

func TestBytesAndInputsHashDeterministic(t *testing.T) {
	h1 := BytesSHA256([]byte("segment-1"))
	h2 := BytesSHA256([]byte("segment-1"))
	if h1 != h2 || len(h1) != 64 {
		t.Fatalf("hash=%q/%q", h1, h2)
	}
	a := InputsHash(map[string]string{"b": "2", "a": "1"})
	b := InputsHash(map[string]string{"a": "1", "b": "2"})
	if a != b {
		t.Fatal("input hash order sensitive")
	}
	c := InputsHash(map[string]string{"a": "1", "b": "3"})
	if a == c {
		t.Fatal("input change did not change hash")
	}
	if !VerifyBytes([]byte("segment-1"), h1) {
		t.Fatal("verify failed")
	}
}
