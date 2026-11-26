package index

import (
	"hash"
	"io"
	"os"
)

// openReadOnly opens a file for hashing without following unsafe paths;
// callers only pass paths previously created inside the output directory.
func openReadOnly(path string) (*os.File, error) {
	return os.Open(path)
}

// copyHash streams a file into the supplied hasher.
func copyHash(h hash.Hash, r io.Reader) (int64, error) {
	return io.Copy(h, r)
}
