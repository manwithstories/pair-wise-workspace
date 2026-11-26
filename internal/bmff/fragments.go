package bmff

import (
	"mediad/internal/model"
)

const (
	tfhdBaseDataOffset = 0x000001
	tfhdDefaultDur     = 0x000008
	tfhdDefaultSize    = 0x000010
	tfhdDefaultFlags   = 0x000020
	tfhdBaseIsMoof     = 0x020000

	trunDataOffset = 0x000001
	trunFirstFlags = 0x000004
	trunDuration   = 0x000100
	trunSize       = 0x000200
	trunFlags      = 0x000400
	trunCTS        = 0x000800
)

type trexDefaults struct {
	Duration uint32
	Size     uint32
}

func (s *Reader) parseFragments(idx *Index, moofs []BoxHeader) error {
	tracks := make(map[uint32]*Track, len(idx.Tracks))
	for i := range idx.Tracks {
		tracks[idx.Tracks[i].ID] = &idx.Tracks[i]
	}
	trex := map[uint32]trexDefaults{}
	if tb, _ := s.FindPath("moov", "mvex", "trex"); tb != nil {
		p, err := s.ReadPayload(tb)
		if err != nil {
			return err
		}
		if len(p) >= 20 {
			trex[u32(p, 4)] = trexDefaults{Duration: u32(p, 12), Size: u32(p, 16)}
		}
	}

	for mi := range moofs {
		moof := &moofs[mi]
		children, err := s.Children(moof)
		if err != nil {
			return err
		}
		for ci := range children {
			if children[ci].Type != "traf" {
				continue
			}
			if err := s.parseTraf(moof, &children[ci], tracks, trex); err != nil {
				return err
			}
		}
	}
	for _, t := range idx.Tracks {
		if len(t.Samples) == 0 {
			return model.New(model.CodeCorruptBox, model.StageTables, "fragmented track has no samples").
				WithSource(s.Source).WithTrack(t.ID)
		}
	}
	return nil
}

func (s *Reader) parseTraf(moof, traf *BoxHeader, tracks map[uint32]*Track, trex map[uint32]trexDefaults) error {
	tfhd, err := s.MustChild(traf, "tfhd")
	if err != nil {
		return err
	}
	tp, err := s.ReadPayload(tfhd)
	if err != nil {
		return err
	}
	if len(tp) < 8 {
		return corruptBox(s.Source, tfhd, "short tfhd")
	}
	_, flags, err := FullBoxVersionFlags(tp)
	if err != nil {
		return corruptBox(s.Source, tfhd, "invalid tfhd full header")
	}
	trackID := u32(tp, 4)
	track, ok := tracks[trackID]
	if !ok {
		return model.New(model.CodeCorruptBox, model.StageTables, "tfhd references unknown track").
			WithSource(s.Source).WithTrack(trackID).WithBox("tfhd").WithOffset(tfhd.Offset)
	}
	pos := 8
	var baseDataOffset int64
	hasBaseOffset := flags&tfhdBaseDataOffset != 0
	if hasBaseOffset {
		if pos+8 > len(tp) {
			return corruptBox(s.Source, tfhd, "truncated tfhd base offset")
		}
		baseDataOffset = int64(u64(tp, pos))
		pos += 8
	}
	if flags&0x000002 != 0 {
		pos += 4
	}
	defaultDur := trex[trackID].Duration
	defaultSize := trex[trackID].Size
	if flags&tfhdDefaultDur != 0 {
		defaultDur = u32(tp, pos)
		pos += 4
	}
	if flags&tfhdDefaultSize != 0 {
		defaultSize = u32(tp, pos)
		pos += 4
	}
	if flags&tfhdDefaultFlags != 0 {
		pos += 4
	}
	base := moof.Offset
	if hasBaseOffset {
		base = baseDataOffset
	} else if flags&tfhdBaseIsMoof != 0 {
		base = moof.Offset
	}

	var baseDTS int64
	if tfdt, _ := s.FirstChild(traf, "tfdt"); tfdt != nil {
		fp, ferr := s.ReadPayload(tfdt)
		if ferr != nil {
			return ferr
		}
		if len(fp) < 8 {
			return corruptBox(s.Source, tfdt, "short tfdt")
		}
		if fp[0] == 1 {
			baseDTS = int64(u64(fp, 4))
		} else {
			baseDTS = int64(u32(fp, 4))
		}
	}

	trun, err := s.MustChild(traf, "trun")
	if err != nil {
		return model.Wrap(model.CodeCorruptBox, model.StageTables, "missing trun", err).WithTrack(trackID)
	}
	rp, err := s.ReadPayload(trun)
	if err != nil {
		return err
	}
	if len(rp) < 8 {
		return corruptBox(s.Source, trun, "short trun").WithTrack(trackID)
	}
	version, tflags, err := FullBoxVersionFlags(rp)
	if err != nil {
		return corruptBox(s.Source, trun, "invalid trun header")
	}
	count := int(u32(rp, 4))
	pos = 8
	var dataOffset int64
	if tflags&trunDataOffset != 0 {
		if pos+4 > len(rp) {
			return corruptBox(s.Source, trun, "truncated trun data offset")
		}
		dataOffset = int64(int32(u32(rp, pos)))
		pos += 4
	}
	if tflags&trunFirstFlags != 0 {
		pos += 4
	}
	dataPos := base + dataOffset
	dts := baseDTS
	for i := 0; i < count; i++ {
		dur, size := defaultDur, defaultSize
		var cts int64
		if tflags&trunDuration != 0 {
			dur = u32(rp, pos)
			pos += 4
		}
		if tflags&trunSize != 0 {
			size = u32(rp, pos)
			pos += 4
		}
		if tflags&trunFlags != 0 {
			pos += 4
		}
		if tflags&trunCTS != 0 {
			if version == 1 {
				cts = int64(int32(u32(rp, pos)))
			} else {
				cts = int64(u32(rp, pos))
			}
			pos += 4
		}
		end := dataPos + int64(size)
		if size == 0 || dataPos < 0 || end > s.Size {
			return model.Newf(model.CodeCorruptBox, model.StageTables,
				"fragment sample %d range %d-%d invalid", i, dataPos, end).
				WithSource(s.Source).WithTrack(trackID).WithBox("trun").WithOffset(trun.Offset)
		}
		track.Samples = append(track.Samples, Sample{
			Index:    len(track.Samples),
			TrackID:  trackID,
			Offset:   dataPos,
			Size:     size,
			DTS:      dts,
			PTS:      dts + cts,
			Duration: dur,
			Sync:     true,
		})
		dts += int64(dur)
		dataPos = end
	}
	return nil
}
