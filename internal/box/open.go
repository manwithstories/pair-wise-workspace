package box

import (
	"fmt"
	"os"
)

// OpenBounded opens path read-only and returns it along with its size.
// Files larger than maxBytes are rejected before any read.
func OpenBounded(path string, maxBytes int64) (*os.File, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, 0, fmt.Errorf("not a regular file: %s", path)
	}
	if maxBytes > 0 && fi.Size() > maxBytes {
		f.Close()
		return nil, 0, &StructureError{Offset: 0, Reason: fmt.Sprintf("file size %d exceeds limit %d", fi.Size(), maxBytes)}
	}
	return f, fi.Size(), nil
}
