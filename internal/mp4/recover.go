package mp4

import (
	"fmt"
	"io"
	"os"

	"mediasim/internal/clock"
)

// Recovered is one media boundary found by scanning, independent of any
// sample table.
type Recovered struct {
	Source  string `json:"source"`
	Index   int    `json:"index"`
	Offset  int64  `json:"offset"`
	Size    uint32 `json:"size"`
	Sync    bool   `json:"sync"`
	InStbl  bool   `json:"in_stbl"` // also present in the intact sample table
	Phantom bool   `json:"phantom"` // overlaps an already committed sample
}

// Recovery is the outcome of scanning one file.
type Recovery struct {
	Path       string      `json:"path"`
	Scanned    [][2]int64  `json:"scanned_mdat"`
	Boundaries []Recovered `json:"boundaries"`
	Skipped    [][2]int64  `json:"skipped_ranges"`
	Resynced   bool        `json:"resynced"`
}

// Recover scans every mdat for MSMP headers and reconstructs sample
// boundaries. It never trusts the sample table: an entry already known to
// the index is marked InStbl so the caller can deduplicate, but the result
// is stable whether the index exists or not.
func Recover(path string, info *Info) (*Recovery, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, clock.NewError(clock.CatContainer, clock.Span{}, clock.RecoveredNone, "open "+path, err)
	}
	defer f.Close()
	rec := &Recovery{Path: path}
	if !info.Synthetic {
		return rec, nil // cannot recognise boundaries in foreign media
	}
	known := map[int64]int{}
	for _, tr := range info.Tracks {
		for _, s := range tr.Samples {
			if s.Valid {
				known[s.Offset] = s.Index
			}
		}
	}
	for _, m := range info.Mdat {
		if m[1] > info.Size {
			m[1] = info.Size // truncated containers report a clipped mdat
		}
		rec.Scanned = append(rec.Scanned, m)
		if err := rec.scanMdat(f, m, known); err != nil {
			return nil, clock.NewError(clock.CatContainer, clock.Span{}, clock.RecoveredResync,
				"scan mdat", err)
		}
	}
	rec.markPhantoms()
	if len(rec.Boundaries) > len(known) {
		rec.Resynced = true
	}
	return rec, nil
}

// scanMdat walks one mdat payload looking for the synthetic header. The MSMP
// header carries its source, index and size so a boundary is self-describing;
// the size is cross-checked against the next header when possible.
func (r *Recovery) scanMdat(f *os.File, m [2]int64, known map[int64]int) error {
	const chunk = 1 << 20
	buf := make([]byte, chunk)
	pos := m[0]
	carry := 0
	for pos < m[1] {
		n := min64(int64(chunk-carry), m[1]-pos)
		if n <= 0 {
			break
		}
		if _, err := f.ReadAt(buf[carry:carry+int(n)], pos); err != nil && err != io.EOF {
			return err
		}
		total := carry + int(n)
		off := 0
		for {
			i := indexMagic(buf[off:total])
			if i < 0 {
				carry = copy(buf[:], buf[total-3:total])
				if total < 3 {
					carry = total
				}
				break
			}
			abs := pos - int64(carry) + int64(off+i)
			hd, ok := parseHeader(buf[off+i : total])
			if !ok || hd.size < HeaderLenOf {
				off += i + 1
				continue
			}
			b := Recovered{
				Source: hd.source, Index: hd.index, Offset: abs, Size: hd.size, Sync: hd.sync,
			}
			if abs+int64(hd.size) > m[1] {
				// The sample runs past mdat: keep the boundary, mark it cut.
				r.Skipped = append(r.Skipped, [2]int64{abs, m[1]})
				break
			}
			if _, ok := known[abs]; ok {
				b.InStbl = true
			}
			r.Boundaries = append(r.Boundaries, b)
			off += i + int(hd.size)
		}
		pos += n
	}
	return nil
}

// HeaderLenOf is the fixed synthetic header length re-exported for scanners.
const HeaderLenOf = 16

type scanHeader struct {
	seed   uint32
	source string
	index  int
	size   uint32
	sync   bool
}

// parseHeader decodes an MSMP header sitting at the start of buf. Layout:
// magic(4) flags(1) seed(2) nameLen(1) indexLo(2) size(4) indexHi(1) body(1).
func parseHeader(buf []byte) (scanHeader, bool) {
	if len(buf) < HeaderLenOf {
		return scanHeader{}, false
	}
	if buf[4] != 0 && buf[4] != 1 {
		return scanHeader{}, false
	}
	nameLen := int(buf[12])
	if nameLen == 0 || nameLen > 8 || HeaderLenOf+nameLen > len(buf) {
		return scanHeader{}, false
	}
	size := uint32(buf[5])<<24 | uint32(buf[6])<<16 | uint32(buf[7])<<8 | uint32(buf[8])
	if size < HeaderLenOf || size > 1<<20 {
		return scanHeader{}, false
	}
	idx := int(buf[9])<<8 | int(buf[10]) | int(buf[11])<<16
	return scanHeader{
		seed:   uint32(buf[13])<<8 | uint32(buf[14]),
		source: string(buf[HeaderLenOf : HeaderLenOf+nameLen]),
		index:  idx,
		size:   size,
		sync:   buf[4] == 1,
	}, true
}

func indexMagic(b []byte) int {
	for i := 0; i+4 <= len(b); i++ {
		if b[i] == 'M' && b[i+1] == 'S' && b[i+2] == 'M' && b[i+3] == 'P' {
			return i
		}
	}
	return -1
}

// markPhantoms flags boundaries that overlap an already committed sample so
// a recovery pass cannot double count frames.
func (r *Recovery) markPhantoms() {
	mine := map[int64]int{}
	for i := range r.Boundaries {
		b := &r.Boundaries[i]
		if prev, ok := mine[b.Offset]; ok {
			_ = prev
			b.Phantom = true
			continue
		}
		mine[b.Offset] = i
	}
	// Deduplicate by (source,index) keeping the first physical occurrence.
	seen := map[string]struct{}{}
	for i := range r.Boundaries {
		key := fmt.Sprintf("%s:%d", r.Boundaries[i].Source, r.Boundaries[i].Index)
		if _, dup := seen[key]; dup {
			r.Boundaries[i].Phantom = true
			continue
		}
		seen[key] = struct{}{}
	}
}

// Committing returns the boundaries that may be rendered: parsed from the
// sample table or found by scan, but never a phantom duplicate.
func (r *Recovery) Committing() []Recovered {
	out := make([]Recovered, 0, len(r.Boundaries))
	for _, b := range r.Boundaries {
		if !b.Phantom {
			out = append(out, b)
		}
	}
	return out
}

// Counts returns totals for the report.
func (r *Recovery) Counts() (total, committed, phantom int) {
	total = len(r.Boundaries)
	committed = len(r.Committing())
	return total, committed, total - committed
}
