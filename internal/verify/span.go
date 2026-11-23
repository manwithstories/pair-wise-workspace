package verify

import (
	"fmt"
	"io"

	"archivecheck/internal/fault"
	"archivecheck/internal/index"
)

// ScanSpan independently re-reads a contiguous byte range of the output
// container and returns the index entries it encodes. It is used by the
// verify stage after every committed fragment so a write fault is caught
// before the checkpoint advances. The span must contain whole records.
func ScanSpan(r io.ReaderAt, start, end int64, path string) ([]index.Entry, error) {
	if end < start {
		return nil, fault.New(fault.KindOutput, path, "", start, "invalid span bounds")
	}
	size := end - start
	if size > 64<<20 {
		return nil, fault.New(fault.KindOutput, path, "", start, "span too large to re-read")
	}
	buf := make([]byte, size)
	if n, err := r.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil, fault.Wrap(err, fault.KindOutput, path, "", start, "cannot re-read committed span")
	} else if int64(n) != size {
		return nil, fault.New(fault.KindShortRead, path, "", start,
			fmt.Sprintf("committed span short: got %d of %d", n, size))
	}
	var entries []index.Entry
	pos := 0
	for pos < len(buf) {
		if buf[pos] != tagFragment {
			return nil, fault.New(fault.KindOutput, path, "", start+int64(pos),
				fmt.Sprintf("unexpected record tag 0x%02x in committed span", buf[pos]))
		}
		rec, next, err := scanRecord(buf, pos+1, path)
		if err != nil {
			return nil, err
		}
		for _, s := range rec.Samples {
			entries = append(entries, index.Entry{
				Segment: rec.Segment, Sequence: rec.Sequence, TrackID: rec.TrackID,
				Timescale: rec.Timescale, DTS: s.DTS, EndDTS: s.EndDTS,
				Offset: s.Offset + start, Size: s.Size, Sync: s.Flags&0x00010000 == 0,
			})
		}
		pos = next
	}
	return entries, nil
}

// Verify entries written for one fragment match what an independent re-read
// produced: same count, timestamps, byte offsets and sizes.
func Verify(expected, actual []index.Entry) error {
	if len(expected) != len(actual) {
		return fault.New(fault.KindOutput, "", "", 0,
			fmt.Sprintf("re-read produced %d entries, wrote %d", len(actual), len(expected)))
	}
	for i := range expected {
		x, y := expected[i], actual[i]
		if x.DTS != y.DTS || x.EndDTS != y.EndDTS || x.Offset != y.Offset ||
			x.Size != y.Size || x.TrackID != y.TrackID || x.Segment != y.Segment {
			return fault.New(fault.KindOutput, x.Segment, "", x.Offset,
				fmt.Sprintf("re-read entry %d disagrees with committed entry", i))
		}
	}
	return nil
}
