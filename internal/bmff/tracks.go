package bmff

import (
	"mediad/internal/model"
)

func (s *Reader) parseTrak(trak *BoxHeader, parseSamples bool) (*Track, error) {
	t := &Track{}
	tkhd, err := s.MustChild(trak, "tkhd")
	if err != nil {
		return nil, err
	}
	p, err := s.ReadPayload(tkhd)
	if err != nil {
		return nil, err
	}
	if len(p) < 16 {
		return nil, corruptBox(s.Source, tkhd, "short tkhd")
	}
	if p[0] == 1 {
		if len(p) < 24 {
			return nil, corruptBox(s.Source, tkhd, "short 64-bit tkhd")
		}
		t.ID = u32(p, 20)
	} else {
		t.ID = u32(p, 12)
	}
	if len(p) >= 80 {
		t.Width = u16(p, len(p)-8)
		t.Height = u16(p, len(p)-4)
	}

	mdia, err := s.MustChild(trak, "mdia")
	if err != nil {
		return nil, model.Wrap(model.CodeCorruptBox, model.StageTables, "missing mdia", err).WithTrack(t.ID)
	}
	mdhd, err := s.MustChild(mdia, "mdhd")
	if err != nil {
		return nil, err
	}
	mp, err := s.ReadPayload(mdhd)
	if err != nil {
		return nil, err
	}
	if len(mp) < 20 {
		return nil, corruptBox(s.Source, mdhd, "short mdhd")
	}
	if mp[0] == 1 {
		if len(mp) < 32 {
			return nil, corruptBox(s.Source, mdhd, "short 64-bit mdhd")
		}
		t.Timescale = u32(mp, 20)
		t.Duration = u64(mp, 24)
	} else {
		t.Timescale = u32(mp, 12)
		t.Duration = uint64(u32(mp, 16))
	}
	if t.Timescale == 0 {
		return nil, model.New(model.CodeCorruptBox, model.StageTables, "zero media timescale").
			WithSource(s.Source).WithTrack(t.ID).WithBox("mdhd").WithOffset(mdhd.Offset)
	}

	if hdlr, _ := s.FirstChild(mdia, "hdlr"); hdlr != nil {
		hp, herr := s.ReadPayload(hdlr)
		if herr != nil {
			return nil, herr
		}
		if len(hp) >= 12 {
			t.Handler = string(hp[8:12])
		}
	}
	if edts, _ := s.FirstChild(trak, "edts"); edts != nil {
		if elst, _ := s.FirstChild(edts, "elst"); elst != nil {
			edits, eerr := s.parseELST(elst, t.ID)
			if eerr != nil {
				return nil, eerr
			}
			t.Edits = edits
		}
	}

	minf, err := s.MustChild(mdia, "minf")
	if err != nil {
		return nil, err
	}
	stbl, err := s.MustChild(minf, "stbl")
	if err != nil {
		return nil, err
	}
	stsd, err := s.FirstChild(stbl, "stsd")
	if err != nil {
		return nil, err
	}
	if stsd != nil {
		if err := s.parseStsd(stsd, t); err != nil {
			return nil, err
		}
	}
	if parseSamples {
		if err := s.parseStblTablesOnly(stbl, t); err != nil {
			return nil, err
		}
	}
	return t, nil
}

func (s *Reader) parseELST(b *BoxHeader, trackID uint32) ([]Edit, error) {
	p, err := s.ReadPayload(b)
	if err != nil {
		return nil, err
	}
	if len(p) < 8 {
		return nil, corruptBox(s.Source, b, "short elst")
	}
	version := p[0]
	count := u32(p, 4)
	pos := 8
	out := make([]Edit, 0, count)
	for i := uint32(0); i < count; i++ {
		e := Edit{}
		if version == 1 {
			if pos+20 > len(p) {
				return nil, corruptBox(s.Source, b, "truncated 64-bit elst entry")
			}
			e.SegmentDuration = int64(u64(p, pos))
			e.MediaTime = int64(u64(p, pos+8))
			e.MediaRateInt = int16(u16(p, pos+16))
			e.MediaRateFrac = int16(u16(p, pos+18))
			pos += 20
		} else {
			if pos+12 > len(p) {
				return nil, corruptBox(s.Source, b, "truncated elst entry")
			}
			e.SegmentDuration = int64(u32(p, pos))
			e.MediaTime = int64(int32(u32(p, pos+4)))
			e.MediaRateInt = int16(u16(p, pos+8))
			e.MediaRateFrac = int16(u16(p, pos+10))
			pos += 12
		}
		out = append(out, e)
	}
	return out, nil
}
