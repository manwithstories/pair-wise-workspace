package mp4atom

import (
	"io"

	"mp4replay/internal/boxscan"
	"mp4replay/internal/report"
)

func parseStts(ra io.ReaderAt, stbl *boxscan.Box, t *Table) error {
	b := boxscan.Child(stbl, "stts")
	if b == nil {
		return report.NewError(report.ExitBoxInvalid, "parse", "stts missing")
	}
	r, err := loadBox(ra, b)
	if err != nil {
		return err
	}
	if _, _, err = r.fullBox(); err != nil {
		return err
	}
	n, err := r.u32()
	if err != nil {
		return err
	}
	if r.left() < int(n)*8 {
		return short("stts entries")
	}
	t.Stts = make([]SttsEntry, n)
	for i := range t.Stts {
		c, _ := r.u32()
		d, _ := r.u32()
		t.Stts[i] = SttsEntry{Count: c, Delta: d}
	}
	return nil
}

func parseCtts(ra io.ReaderAt, stbl *boxscan.Box, t *Table) error {
	b := boxscan.Child(stbl, "ctts")
	if b == nil {
		return nil
	}
	r, err := loadBox(ra, b)
	if err != nil {
		return err
	}
	ver, _, err := r.fullBox()
	if err != nil {
		return err
	}
	t.CttsVer = ver
	t.HasCtts = true
	n, err := r.u32()
	if err != nil {
		return err
	}
	if r.left() < int(n)*8 {
		return short("ctts entries")
	}
	t.Ctts = make([]CttsEntry, n)
	for i := range t.Ctts {
		c, _ := r.u32()
		if ver == 0 {
			off, _ := r.u32()
			t.Ctts[i] = CttsEntry{Count: c, Offset: int64(off)}
		} else {
			off, _ := r.i32()
			t.Ctts[i] = CttsEntry{Count: c, Offset: int64(off)}
		}
	}
	return nil
}

func parseStss(ra io.ReaderAt, stbl *boxscan.Box, t *Table) error {
	b := boxscan.Child(stbl, "stss")
	if b == nil {
		return nil
	}
	r, err := loadBox(ra, b)
	if err != nil {
		return err
	}
	if _, _, err = r.fullBox(); err != nil {
		return err
	}
	n, err := r.u32()
	if err != nil {
		return err
	}
	if r.left() < int(n)*4 {
		return short("stss entries")
	}
	t.HasStss = true
	t.Stss = make([]uint32, n)
	for i := range t.Stss {
		t.Stss[i], _ = r.u32()
	}
	return nil
}

func parseStsc(ra io.ReaderAt, stbl *boxscan.Box, t *Table) error {
	b := boxscan.Child(stbl, "stsc")
	if b == nil {
		return report.NewError(report.ExitBoxInvalid, "parse", "stsc missing")
	}
	r, err := loadBox(ra, b)
	if err != nil {
		return err
	}
	if _, _, err = r.fullBox(); err != nil {
		return err
	}
	n, err := r.u32()
	if err != nil {
		return err
	}
	if r.left() < int(n)*12 {
		return short("stsc entries")
	}
	t.Stsc = make([]StscEntry, n)
	for i := range t.Stsc {
		fc, _ := r.u32()
		spc, _ := r.u32()
		di, _ := r.u32()
		t.Stsc[i] = StscEntry{FirstChunk: fc, SamplesPerChunk: spc, DescIndex: di}
	}
	return nil
}

func parseStsz(ra io.ReaderAt, stbl *boxscan.Box, t *Table) error {
	b := boxscan.Child(stbl, "stsz")
	if b == nil {
		return report.NewError(report.ExitBoxInvalid, "parse", "stsz missing")
	}
	r, err := loadBox(ra, b)
	if err != nil {
		return err
	}
	if _, _, err = r.fullBox(); err != nil {
		return err
	}
	if t.StszFixed, err = r.u32(); err != nil {
		return err
	}
	if t.StszCount, err = r.u32(); err != nil {
		return err
	}
	if t.StszFixed != 0 {
		return nil
	}
	if r.left() < int(t.StszCount)*4 {
		return short("stsz entries")
	}
	t.StszSizes = make([]uint32, t.StszCount)
	for i := range t.StszSizes {
		t.StszSizes[i], _ = r.u32()
	}
	return nil
}

func parseOffsets(ra io.ReaderAt, stbl *boxscan.Box, t *Table) error {
	if co := boxscan.Child(stbl, "co64"); co != nil {
		r, err := loadBox(ra, co)
		if err != nil {
			return err
		}
		if _, _, err = r.fullBox(); err != nil {
			return err
		}
		n, err := r.u32()
		if err != nil {
			return err
		}
		if r.left() < int(n)*8 {
			return short("co64 entries")
		}
		t.Stco = make([]int64, n)
		for i := range t.Stco {
			v, _ := r.u64()
			t.Stco[i] = int64(v)
		}
		return nil
	}
	b := boxscan.Child(stbl, "stco")
	if b == nil {
		return report.NewError(report.ExitBoxInvalid, "parse", "stco/co64 missing")
	}
	r, err := loadBox(ra, b)
	if err != nil {
		return err
	}
	if _, _, err = r.fullBox(); err != nil {
		return err
	}
	n, err := r.u32()
	if err != nil {
		return err
	}
	if r.left() < int(n)*4 {
		return short("stco entries")
	}
	t.Stco = make([]int64, n)
	for i := range t.Stco {
		v, _ := r.u32()
		t.Stco[i] = int64(v)
	}
	return nil
}
