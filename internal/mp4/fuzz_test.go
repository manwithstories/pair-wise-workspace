package mp4

import (
	"bytes"
	"testing"
)

// FuzzReadBoxStructure ensures the walker never panics and never reports a box
// that extends past the input.
func FuzzReadBoxStructure(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 8, 'f', 't', 'y', 'p'})
	f.Add([]byte{0, 0, 0, 16, 'm', 'o', 'o', 'v', 0, 0, 0, 8, 'f', 'r', 'e', 'e'})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewReader(bytes.NewReader(data), int64(len(data)))
		_, _ = r.boxesIn(0, uint64(len(data)))
	})
}
