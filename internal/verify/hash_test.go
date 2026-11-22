package verify

import "testing"

func TestHashAndCheck(t *testing.T) {
	contents := [][]byte{[]byte("seg-0"), []byte("seg-1"), []byte("seg-2")}
	hashes := HashSegments(contents)
	if len(hashes) != 3 || hashes[0] == hashes[1] {
		t.Fatalf("hashes = %v", hashes)
	}
	if bad := CheckSegments(contents, hashes); len(bad) != 0 {
		t.Fatalf("unexpected mismatches: %v", bad)
	}
	// Corrupt segment 1: mismatch must be located by index.
	contents[1] = []byte("tampered")
	bad := CheckSegments(contents, hashes)
	if len(bad) != 1 || bad[0].Segment != 1 {
		t.Fatalf("mismatches = %v, want segment 1", bad)
	}
	want := "verify: segment 1 hash mismatch"
	if got := bad[0].Error(); got[:len(want)] != want {
		t.Fatalf("error = %q", got)
	}
}

func TestSum256Stable(t *testing.T) {
	if Sum256([]byte("abc")) != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal("SHA-256 of abc changed")
	}
}
