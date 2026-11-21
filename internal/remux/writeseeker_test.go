package remux

import (
	"io"
	"testing"
)

// TestWriteSeekerContract guards the in-memory WriteSeeker used by the
// go-mp4 sidecar writer for box-size patching.
func TestWriteSeekerContract(t *testing.T) {
	var ws writeSeeker
	if n, err := ws.Write([]byte{1, 2, 3, 4}); err != nil || n != 4 {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	pos, err := ws.Seek(-2, io.SeekEnd)
	if err != nil || pos != 2 {
		t.Fatalf("seek end: pos=%d err=%v", pos, err)
	}
	if n, err := ws.Write([]byte{9, 9}); err != nil || n != 2 {
		t.Fatalf("overwrite: %d %v", n, err)
	}
	want := []byte{1, 2, 9, 9}
	for i, b := range ws.Bytes() {
		if b != want[i] {
			t.Fatalf("bytes mismatch: % x", ws.Bytes())
		}
	}
	if _, err := ws.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("negative seek must be rejected")
	}
}
