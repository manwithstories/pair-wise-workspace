package index

import (
	"os"

	"github.com/abema/go-mp4"
)

// ReadEntries reconstructs index entries from an MP4's sample table so the
// standalone verify command can seek without the in-memory pipeline result.
// Sequence numbers are inferred from sample order (0-based); offsets point at
// the mdat bytes rather than the original JSONL, which is sufficient for
// read-back seek validation.
func ReadEntries(f *os.File) ([]Entry, error) {
	stbl := mustStbl(f)
	if stbl == nil {
		return nil, errStblMissing
	}
	stts := extractOne[*mp4.Stts](f, stbl, mp4.BoxTypeStts())
	stsz := extractOne[*mp4.Stsz](f, stbl, mp4.BoxTypeStsz())
	if stts == nil || stsz == nil {
		return nil, errStblMissing
	}
	n := int(stsz.SampleCount)
	entries := make([]Entry, 0, n)
	var t uint64
	sample := 0
	for _, run := range stts.Entries {
		for k := uint32(0); k < run.SampleCount && sample < n; k++ {
			var size uint32
			if stsz.SampleSize != 0 {
				size = stsz.SampleSize
			} else if sample < len(stsz.EntrySize) {
				size = stsz.EntrySize[sample]
			}
			entries = append(entries, Entry{
				Seq: uint64(sample), MasterTS: t, ChunkIndex: uint32(sample),
				PayloadLen: size, KeyFrame: true,
			})
			t += uint64(run.SampleDelta)
			sample++
		}
	}
	return entries, nil
}

var errStblMissing = stblError{"sample table boxes missing or unreadable"}

type stblError struct{ msg string }

func (e stblError) Error() string { return e.msg }
