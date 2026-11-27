package wire

import (
	"encoding/binary"

	"relayaudit/internal/fault"
)

const (
	// PageMagic identifies an index page.
	PageMagic = "IDX1"
	// PageHeaderSize is magic(4) version(1) reserved(2) count(2).
	PageHeaderSize = 8
	// EntrySize is offset(8) length(4) track(1) seqLow(2) flags(1).
	EntrySize = 16
	// PageCapacity is the fixed number of entry slots per index page.
	PageCapacity = 64
	// PageSize is the encoded size of a full index page.
	PageSize = PageHeaderSize + PageCapacity*EntrySize
)

// Entry is one index page slot.
type Entry struct {
	Offset int64
	Length uint32
	Track  uint8
	SeqLow uint16
	Flags  uint8
}

// Index is a decoded index.
type Index struct {
	Entries []Entry
	Pages   int
	// Rebuilt marks an index produced by scanning rather than read from disk.
	Rebuilt bool
}

// Find returns the entry covering off, if any.
func (idx *Index) Find(off int64) (Entry, bool) {
	lo, hi := 0, len(idx.Entries)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		e := idx.Entries[mid]
		switch {
		case off < e.Offset:
			hi = mid - 1
		case off >= e.Offset+int64(e.Length):
			lo = mid + 1
		default:
			return e, true
		}
	}
	return Entry{}, false
}

// EncodeIndex lays entries out into fixed-size pages.
func EncodeIndex(entries []Entry) []byte {
	pages := (len(entries) + PageCapacity - 1) / PageCapacity
	out := make([]byte, pages*PageSize)
	for p := 0; p < pages; p++ {
		base := p * PageSize
		copy(out[base:base+4], PageMagic)
		out[base+4] = Version
		start := p * PageCapacity
		n := len(entries) - start
		if n > PageCapacity {
			n = PageCapacity
		}
		binary.BigEndian.PutUint16(out[base+6:base+8], uint16(n))
		for i := 0; i < n; i++ {
			e := entries[start+i]
			slot := out[base+PageHeaderSize+i*EntrySize:]
			binary.BigEndian.PutUint64(slot[0:8], uint64(e.Offset))
			binary.BigEndian.PutUint32(slot[8:12], e.Length)
			slot[12] = e.Track
			binary.BigEndian.PutUint16(slot[13:15], e.SeqLow)
			slot[15] = e.Flags
		}
	}
	return out
}

// PagesFor returns the index page count needed for n entries.
func PagesFor(n int) int {
	if n <= 0 {
		return 0
	}
	return (n + PageCapacity - 1) / PageCapacity
}

// ParseIndex decodes the index region [HeaderSize, HeaderSize+PageCount*PageSize)
// and checks structural invariants: page magic, declared counts, in-bounds and
// strictly increasing offsets.
//
// A missing or truncated region is reported as an index fault, which callers
// treat as a signal to rebuild from records rather than trust a cache.
func ParseIndex(buf []byte, h Header) (*Index, error) {
	if h.PageCount == 0 {
		return nil, fault.New(fault.Index, HeaderSize, "segment has no index pages")
	}
	regionEnd := int64(HeaderSize) + int64(h.PageCount)*PageSize
	if regionEnd > int64(len(buf)) {
		return nil, fault.New(fault.Index, int64(len(buf)),
			"index region truncated: need %d bytes, file has %d", regionEnd, len(buf))
	}
	idx := &Index{Pages: int(h.PageCount)}
	for p := 0; p < int(h.PageCount); p++ {
		base := int64(HeaderSize) + int64(p)*PageSize
		if string(buf[base:base+4]) != PageMagic {
			return nil, fault.New(fault.Index, base, "index page %d has bad magic", p)
		}
		if buf[base+4] != Version {
			return nil, fault.New(fault.Index, base+4, "index page %d version %d", p, buf[base+4])
		}
		n := int(binary.BigEndian.Uint16(buf[base+6 : base+8]))
		if n > PageCapacity {
			return nil, fault.New(fault.Index, base+6, "index page %d claims %d entries", p, n)
		}
		for i := 0; i < n; i++ {
			slotOff := base + PageHeaderSize + int64(i)*EntrySize
			slot := buf[slotOff : slotOff+EntrySize]
			e := Entry{
				Offset: int64(binary.BigEndian.Uint64(slot[0:8])),
				Length: binary.BigEndian.Uint32(slot[8:12]),
				Track:  slot[12],
				SeqLow: binary.BigEndian.Uint16(slot[13:15]),
				Flags:  slot[15],
			}
			if e.Length == 0 || e.Length > MaxRecordSize {
				return nil, fault.New(fault.Index, slotOff, "index entry %d has illegal length %d", len(idx.Entries), e.Length)
			}
			if e.Offset < h.DataStart {
				return nil, fault.New(fault.Index, slotOff,
					"index entry %d offset %d points inside the index region", len(idx.Entries), e.Offset)
			}
			if e.Offset+int64(e.Length) > int64(len(buf)) {
				return nil, fault.New(fault.Index, slotOff,
					"index entry %d ends at %d beyond file of %d bytes", len(idx.Entries), e.Offset+int64(e.Length), len(buf))
			}
			if e.Track == 0 || e.Track > MaxTracks {
				return nil, fault.New(fault.Index, slotOff, "index entry %d has illegal track %d", len(idx.Entries), e.Track)
			}
			idx.Entries = append(idx.Entries, e)
		}
	}
	// Entries must rise strictly and may not overlap once every page has
	// been decoded; checking here keeps page-local order checks independent.
	for i := 1; i < len(idx.Entries); i++ {
		prev := idx.Entries[i-1]
		if idx.Entries[i].Offset < prev.Offset+int64(prev.Length) {
			return nil, fault.New(fault.Index, idx.Entries[i].Offset,
				"index entry %d overlaps its predecessor", i)
		}
	}
	return idx, nil
}

// VerifyIndex cross-checks every index entry against the bytes it points at.
// A stale or mismatched index must not be trusted; the returned fault is an
// index fault at the first divergent entry.
func VerifyIndex(idx *Index, buf []byte, h Header) error {
	if idx == nil {
		return fault.New(fault.Index, HeaderSize, "nil index")
	}
	if uint32(len(idx.Entries)) != h.RecordCount {
		return fault.New(fault.Index, HeaderSize,
			"index lists %d records, header declares %d", len(idx.Entries), h.RecordCount)
	}
	for i, e := range idx.Entries {
		rec, err := readRecordAt(buf, e.Offset, int64(len(buf)))
		if err != nil {
			return fault.Wrap(fault.Index, e.Offset, err, "index entry %d does not decode: %v", i, err)
		}
		if rec.Track != e.Track || rec.Size() != int64(e.Length) || uint16(rec.Seq) != e.SeqLow {
			return fault.New(fault.Index, e.Offset,
				"index entry %d disagrees with record (track %d/%d size %d/%d seq %d/%d)",
				i, e.Track, rec.Track, e.Length, rec.Size(), e.SeqLow, uint16(rec.Seq))
		}
	}
	return nil
}

// readRecordAt decodes exactly one record starting at off.
func readRecordAt(buf []byte, off, end int64) (*Record, error) {
	r := NewRecordReader(buf, off, end)
	rec, err := r.Next()
	if err != nil {
		return nil, err
	}
	return rec, nil
}
