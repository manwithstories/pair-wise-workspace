package index

import (
	"bytes"
	"io"

	mp4 "github.com/abema/go-mp4"
)

// writeSeeker is a minimal in-memory io.WriteSeeker for mp4.Writer.
type writeSeeker struct {
	buf []byte
	off int64
}

func (w *writeSeeker) Write(p []byte) (int, error) {
	end := w.off + int64(len(p))
	if grow := end - int64(len(w.buf)); grow > 0 {
		w.buf = append(w.buf, make([]byte, grow)...)
	}
	copy(w.buf[w.off:end], p)
	w.off = end
	return len(p), nil
}

func (w *writeSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		w.off = offset
	case io.SeekCurrent:
		w.off += offset
	case io.SeekEnd:
		w.off = int64(len(w.buf)) + offset
	}
	return w.off, nil
}

// ExportSyncTable serializes the rebuilt random-access index as MP4 sample
// table boxes (stss sync samples + stts constant-duration timing), the form
// editors consume for fast seeking. Deterministic for a given index.
func ExportSyncTable(idx *Index, sampleDelta uint32) ([]byte, error) {
	stss := &mp4.Stss{SampleNumber: make([]uint32, len(idx.Entries))}
	for i, e := range idx.Entries {
		stss.SampleNumber[i] = uint32(e.Seq) + 1 // MP4 sample numbers are 1-based
	}
	stss.EntryCount = uint32(len(stss.SampleNumber))
	stts := &mp4.Stts{
		EntryCount: 1,
		Entries:    []mp4.SttsEntry{{SampleCount: stss.EntryCount, SampleDelta: sampleDelta}},
	}
	ws := &writeSeeker{}
	w := mp4.NewWriter(ws)
	for _, box := range []mp4.IImmutableBox{stss, stts} {
		if _, err := w.StartBox(&mp4.BoxInfo{Type: box.GetType()}); err != nil {
			return nil, err
		}
		if _, err := mp4.Marshal(w, box, mp4.Context{}); err != nil {
			return nil, err
		}
		if _, err := w.EndBox(); err != nil {
			return nil, err
		}
	}
	return ws.buf, nil
}

// ReadSyncTable parses boxes written by ExportSyncTable back into sample
// numbers, for round-trip verification.
func ReadSyncTable(data []byte) ([]uint32, error) {
	r := bytes.NewReader(data)
	var nums []uint32
	_, err := mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		if h.BoxInfo.Type != mp4.BoxTypeStss() {
			return nil, nil
		}
		box, _, err := h.ReadPayload()
		if err != nil {
			return nil, err
		}
		if stss, ok := box.(*mp4.Stss); ok {
			nums = stss.SampleNumber
		}
		return nil, nil
	})
	return nums, err
}
