package mp4atom

import (
	"io"

	"mp4replay/internal/boxscan"
	"mp4replay/internal/report"
)

// ParseTrak parses every supported leaf box of one trak into a Table.
func ParseTrak(ra io.ReaderAt, trak *boxscan.Box) (*Table, error) {
	t := &Table{}
	mdia := boxscan.Child(trak, "mdia")
	if mdia == nil {
		return nil, report.NewError(report.ExitBoxInvalid, "parse", "mdia missing")
	}
	minf := boxscan.Child(mdia, "minf")
	if minf == nil {
		return nil, report.NewError(report.ExitBoxInvalid, "parse", "minf missing")
	}
	stbl := boxscan.Child(minf, "stbl")
	if stbl == nil {
		return nil, report.NewError(report.ExitBoxInvalid, "parse", "stbl missing")
	}
	if err := parseTkhd(ra, trak, t); err != nil {
		return nil, err
	}
	if err := parseMdhd(ra, mdia, t); err != nil {
		return nil, err
	}
	if err := parseHdlr(ra, mdia, t); err != nil {
		return nil, err
	}
	if err := parseStts(ra, stbl, t); err != nil {
		return nil, err
	}
	if err := parseCtts(ra, stbl, t); err != nil {
		return nil, err
	}
	if err := parseStss(ra, stbl, t); err != nil {
		return nil, err
	}
	if err := parseStsc(ra, stbl, t); err != nil {
		return nil, err
	}
	if err := parseStsz(ra, stbl, t); err != nil {
		return nil, err
	}
	if err := parseOffsets(ra, stbl, t); err != nil {
		return nil, err
	}
	if err := parseElst(ra, trak, t); err != nil {
		return nil, err
	}
	return t, nil
}

func parseTkhd(ra io.ReaderAt, trak *boxscan.Box, t *Table) error {
	b := boxscan.Path(trak, "tkhd")
	if b == nil {
		return report.NewError(report.ExitBoxInvalid, "parse", "tkhd missing")
	}
	r, err := loadBox(ra, b)
	if err != nil {
		return err
	}
	ver, _, err := r.fullBox()
	if err != nil {
		return err
	}
	if ver == 1 {
		if _, err = r.u64(); err != nil {
			return err
		}
		if _, err = r.u64(); err != nil {
			return err
		}
	} else {
		if _, err = r.u32(); err != nil {
			return err
		}
		if _, err = r.u32(); err != nil {
			return err
		}
	}
	t.TrackID, err = r.u32()
	if err != nil {
		return err
	}
	if _, err = r.u32(); err != nil { // reserved
		return err
	}
	return nil
}

func parseMdhd(ra io.ReaderAt, mdia *boxscan.Box, t *Table) error {
	b := boxscan.Child(mdia, "mdhd")
	if b == nil {
		return report.NewError(report.ExitBoxInvalid, "parse", "mdhd missing")
	}
	r, err := loadBox(ra, b)
	if err != nil {
		return err
	}
	ver, _, err := r.fullBox()
	if err != nil {
		return err
	}
	if ver == 1 {
		if _, err = r.u64(); err != nil {
			return err
		}
		if _, err = r.u64(); err != nil {
			return err
		}
	} else {
		if _, err = r.u32(); err != nil {
			return err
		}
		if _, err = r.u32(); err != nil {
			return err
		}
	}
	if t.Timescale, err = r.u32(); err != nil {
		return err
	}
	if ver == 1 {
		t.MediaDur, err = r.u64()
	} else {
		var d uint32
		d, err = r.u32()
		t.MediaDur = uint64(d)
	}
	if err != nil {
		return err
	}
	lang, err := r.u16()
	t.Language = lang
	return err
}

func parseHdlr(ra io.ReaderAt, mdia *boxscan.Box, t *Table) error {
	b := boxscan.Child(mdia, "hdlr")
	if b == nil {
		return report.NewError(report.ExitBoxInvalid, "parse", "hdlr missing")
	}
	r, err := loadBox(ra, b)
	if err != nil {
		return err
	}
	if _, _, err = r.fullBox(); err != nil {
		return err
	}
	if _, err = r.u32(); err != nil { // pre_defined
		return err
	}
	if r.left() < 4 {
		return short("handler_type")
	}
	t.Handler = string(r.buf[r.pos : r.pos+4])
	r.pos += 4
	return nil
}
