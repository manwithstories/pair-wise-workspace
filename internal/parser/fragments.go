package parser

func parseTFDT(src *Source, b *Box, t *Track) {
	r := newTableReader(src, b)
	version, ok := r.u8()
	if !ok {
		return
	}
	_, _ = r.take(3)
	frag := Fragment{TfdtOffset: b.Offset, TrackID: t.TrackID, Version: version}
	if version == 1 {
		v, ok := r.u64()
		if !ok || v > maxInt64Value {
			frag.BaseTime = -1
		} else {
			frag.BaseTime = int64(v)
		}
	} else {
		v, ok := r.u32()
		if ok {
			frag.BaseTime = int64(v)
		}
	}
	t.Fragments = append(t.Fragments, frag)
}

const maxInt64Value = uint64(1<<63 - 1)

func parseTRUN(src *Source, idx *BoxIndex, b *Box, t *Track) {
	r := newTableReader(src, b)
	header, ok := r.take(4)
	if !ok {
		return
	}
	flags := uint32(header[1])<<16 | uint32(header[2])<<8 | uint32(header[3])
	count, ok := r.u32()
	if !ok {
		return
	}
	if count > uint32(r.remaining()/4) {
		// A conservative payload bound; each sample has at least one u32 field
		// in the fixtures and all ordinary trun variants.
		count = uint32(r.remaining() / 4)
	}

	moofOffset := b.Offset
	for _, candidate := range idx.All {
		if candidate.Type == "moof" && candidate.Offset <= b.Offset && candidate.Offset > moofOffset-1_000_000_000 {
			moofOffset = candidate.Offset
		}
	}

	frag := &Fragment{MoofOffset: moofOffset, TrunOffset: b.Offset, TrackID: t.TrackID}
	if len(t.Fragments) > 0 && t.Fragments[len(t.Fragments)-1].TrunOffset == 0 {
		frag = &t.Fragments[len(t.Fragments)-1]
		frag.MoofOffset = moofOffset
		frag.TrunOffset = b.Offset
	} else {
		t.Fragments = append(t.Fragments, *frag)
		frag = &t.Fragments[len(t.Fragments)-1]
	}

	if flags&0x000001 != 0 {
		v, ok := r.u32()
		if ok {
			frag.DataOffset = int32(v)
		}
	}
	var firstFlags uint32
	hasFirstFlags := false
	if flags&0x000004 != 0 {
		firstFlags, ok = r.u32()
		hasFirstFlags = ok
	}

	frag.Samples = make([]FragmentSample, 0, count)
	for i := uint32(0); i < count; i++ {
		s := FragmentSample{Number: int(i) + 1, Keyframe: true}
		if flags&0x000100 != 0 {
			s.Duration, ok = r.u32()
			if !ok {
				break
			}
		}
		if flags&0x000200 != 0 {
			size, ok := r.u32()
			if !ok {
				break
			}
			s.Size = size
		}
		if flags&0x000400 != 0 {
			s.Flags, ok = r.u32()
			if !ok {
				break
			}
			// ISO sample_flags: bit 16 is sample_is_non_sync_sample.
			s.Keyframe = s.Flags&0x00010000 == 0
		} else if i == 0 && hasFirstFlags {
			s.Flags = firstFlags
			s.Keyframe = firstFlags&0x00010000 == 0
		}
		if flags&0x000800 != 0 {
			if _, ok := r.u32(); !ok {
				break
			}
		}
		frag.Samples = append(frag.Samples, s)
	}
}
