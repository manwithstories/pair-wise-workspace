package index

import (
	"io"
	"os"
)

// Open reads an index file fully into memory. It is used for validation and by
// tools that need random access to a small index; the pipeline streams with
// Scan instead when a track's entry count could be large.
func Open(path string) ([]Entry, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	return ReadAll(f)
}

// LookupByFrame returns the entry for a frame number via binary search, or nil
// when the frame is not indexed (for example a corrupt or truncated frame).
func LookupByFrame(entries []Entry, frameNo uint64) *Entry {
	lo, hi := 0, len(entries)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		switch {
		case entries[mid].FrameNo == frameNo:
			e := entries[mid]
			return &e
		case entries[mid].FrameNo < frameNo:
			lo = mid + 1
		default:
			hi = mid - 1
		}
	}
	return nil
}

// OpenScanner opens an index file for streaming reads. The caller closes it.
func OpenScanner(path string) (*os.File, error) { return os.Open(path) }

// CloseScanner closes a scanner returned by OpenScanner.
func CloseScanner(f *os.File) error { return f.Close() }

// ReadAllStream is a small adapter so callers holding an io.ReadSeeker can
// stream entries without an os.File handle.
func ReadAllStream(r io.ReadSeeker, fn func(Entry) error) (int, error) { return Scan(r, fn) }
