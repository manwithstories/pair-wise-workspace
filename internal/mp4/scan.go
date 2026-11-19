package mp4

import "io"

// scanReader is completed with the sample-table decoder in the next change.
func scanReader(r io.ReaderAt, size int64, path string) (*Scan, error) {
	sc := &Scan{Path: path, Bytes: size}
	boxes, truncated, faults := Walk(r, size)
	sc.Truncated = truncated
	sc.Faults.Add(faults...)
	_ = boxes
	return sc, nil
}
