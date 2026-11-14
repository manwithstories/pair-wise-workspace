package mp4atom

import (
	"io"

	"mp4replay/internal/boxscan"
	"mp4replay/internal/report"
)

func parseElst(ra io.ReaderAt, trak *boxscan.Box, t *Table) error {
	edts := boxscan.Child(trak, "edts")
	if edts == nil {
		return nil
	}
	b := boxscan.Child(edts, "elst")
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
	n, err := r.u32()
	if err != nil {
		return err
	}
	t.Elst = make([]Edit, 0, n)
	for i := uint32(0); i < n; i++ {
		e := Edit{}
		if ver == 1 {
			if e.SegmentDuration, err = r.u64(); err != nil {
				return err
			}
			if e.MediaTime, err = r.i64(); err != nil {
				return err
			}
		} else {
			d, err := r.u32()
			if err != nil {
				return err
			}
			mt, err := r.i32()
			if err != nil {
				return err
			}
			e.SegmentDuration, e.MediaTime = uint64(d), int64(mt)
		}
		ri, err := r.i16()
		if err != nil {
			return err
		}
		if _, err = r.u16(); err != nil { // rate fraction, must be 0
			return err
		}
		e.Rate = float64(ri)
		if e.Rate == 0 {
			e.Rate = 1
		}
		t.Elst = append(t.Elst, e)
	}
	return nil
}

// ParseStsd validates the sample description count and returns it. The codec
// payload is opaque to the replay pipeline; only the box presence matters.
func ParseStsd(ra io.ReaderAt, stbl *boxscan.Box) (uint32, error) {
	b := boxscan.Child(stbl, "stsd")
	if b == nil {
		return 0, report.NewError(report.ExitBoxInvalid, "parse", "stsd missing")
	}
	r, err := loadBox(ra, b)
	if err != nil {
		return 0, err
	}
	if _, _, err = r.fullBox(); err != nil {
		return 0, err
	}
	return r.u32()
}
