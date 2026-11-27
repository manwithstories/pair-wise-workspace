package wire

import (
	"errors"
	"io"

	"relayaudit/internal/fault"
)

type IndexRecord struct {
	Entry Entry
	Rec   *Record
}

// Rebuild scans records from h.DataStart to end and constructs a fresh index.
// Damaged spans are skipped with a bounded resync; every damage is reported
// with its original offset. Rebuilding never reads beyond end.
func Rebuild(buf []byte, h Header, end int64) (*Index, []*IndexRecord, []*fault.Fault, error) {
	if end > int64(len(buf)) {
		end = int64(len(buf))
	}
	if h.DataStart >= end {
		return nil, nil, []*fault.Fault{
			fault.New(fault.Index, h.DataStart, "no record data to rebuild from"),
		}, errNoRecords
	}
	rd := NewRecordReader(buf, h.DataStart, end)
	idx := &Index{Rebuilt: true}
	var recs []*IndexRecord
	var damage []*fault.Fault
	for {
		rec, err := rd.Next()
		if err == nil {
			e := Entry{
				Offset: rec.Offset, Length: uint32(rec.Size()),
				Track: rec.Track, SeqLow: uint16(rec.Seq),
			}
			idx.Entries = append(idx.Entries, e)
			recs = append(recs, &IndexRecord{Entry: e, Rec: rec})
			continue
		}
		if isEOF(err) {
			break
		}
		f, _ := fault.As(err)
		if f == nil {
			return nil, nil, damage, err
		}
		damage = append(damage, f)
		if f.Kind() == fault.Truncated && f.Offset() >= end-int64(RecordOverhead) {
			break
		}
		next, rerr := rd.Resync(f.Offset())
		if rerr != nil {
			if rf, ok := fault.As(rerr); ok && rf.Offset() != f.Offset() {
				damage = append(damage, rf)
			}
			break
		}
		rd.off = next
	}
	idx.Pages = PagesFor(len(idx.Entries))
	return idx, recs, damage, nil
}

func isEOF(err error) bool {
	return errors.Is(err, io.EOF)
}
