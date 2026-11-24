package mp4

import (
	"mediad/internal/core"
)

// ParseFragment decodes the generated moof/mdat dialect and validates that
// trun references stay inside the boxes. The returned error is a *core.Fault
// with the first offending byte offset.
func ParseFragment(data []byte) (*Fragment, error) {
	var moofH, mdatH *BoxHeader
	if err := WalkTop(data, func(h BoxHeader) error {
		switch h.Type {
		case BoxMoof:
			if moofH == nil {
				hh := h
				moofH = &hh
			}
		case BoxMdat:
			if mdatH == nil {
				hh := h
				mdatH = &hh
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if moofH == nil {
		return nil, core.Faultf(core.KindBadField, 0, "fragment has no moof box")
	}

	mfhdH, ok, err := FindDirectChild(data, *moofH, BoxMfhd)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, core.Faultf(core.KindBadField, moofH.Offset, "moof has no mfhd")
	}
	seq, err := parseMfhd(data, mfhdH)
	if err != nil {
		return nil, err
	}

	trafH, ok, err := FindDirectChild(data, *moofH, BoxTraf)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, core.Faultf(core.KindBadField, moofH.Offset, "moof has no traf")
	}

	frag := &Fragment{Sequence: seq}
	if tfhdH, found, ferr := FindDirectChild(data, trafH, BoxTfhd); ferr != nil {
		return nil, ferr
	} else if found {
		if frag.TrackID, err = parseTfhdTrack(data, tfhdH); err != nil {
			return nil, err
		}
	}
	if tfdtH, found, ferr := FindDirectChild(data, trafH, BoxTfdt); ferr != nil {
		return nil, ferr
	} else if found {
		frag.BaseDecode, frag.TFDTVersion, err = parseTfdt(data, tfdtH)
		if err != nil {
			return nil, err
		}
	}
	trunH, found, err := FindDirectChild(data, trafH, BoxTrun)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, core.Faultf(core.KindBadField, trafH.Offset, "traf has no trun")
	}
	frag.TrunVersion, frag.TrunFlags, frag.DataOffset, frag.HasDataOffset, frag.Samples, err = parseTrun(data, trunH)
	if err != nil {
		return nil, err
	}

	if mdatH != nil {
		frag.Data = data[mdatH.PayloadOff:mdatH.End]
		if err := validateRun(data, *moofH, *mdatH, frag); err != nil {
			return nil, err
		}
	}
	return frag, nil
}

func validateRun(data []byte, moof, mdat BoxHeader, f *Fragment) error {
	var start int64 = mdat.PayloadOff
	if f.HasDataOffset {
		base := moof.Offset // generated fragments set default-base-is-moof
		cand, ok := core.AddChecked(base, int64(f.DataOffset))
		if !ok || f.DataOffset < 0 {
			return core.Faultf(core.KindOutOfBounds, moof.PayloadOff, "data_offset %d invalid", f.DataOffset)
		}
		start = cand
	}
	if start < mdat.PayloadOff || start > mdat.End {
		return core.Faultf(core.KindOutOfBounds, moof.Offset, "data start %d outside mdat [%d,%d)", start, mdat.PayloadOff, mdat.End)
	}
	var total int64
	for i, s := range f.Samples {
		nt, ok := core.AddChecked(total, int64(s.Size))
		if !ok || nt < 0 {
			return core.Faultf(core.KindOverflow, moof.Offset, "sample %d size sum overflow", i)
		}
		total = nt
	}
	end, ok := core.AddChecked(start, total)
	if !ok || end > mdat.End {
		return core.Faultf(core.KindOutOfBounds, start, "sample bytes end %d (sum=%d) exceed mdat end %d", end, total, mdat.End)
	}
	return nil
}
