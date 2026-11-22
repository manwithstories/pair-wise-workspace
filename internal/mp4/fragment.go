package mp4

import (
	"fmt"

	gomp4 "github.com/abema/go-mp4"
)

// ParseInit parses an init segment into per-track info.
func ParseInit(data []byte) (*InitInfo, error) {
	init := &InitInfo{Tracks: map[uint32]*TrackInfo{}}
	var cur *TrackInfo
	err := walk(data, func(h boxHeader, payload []byte) error {
		switch h.typ {
		case "tkhd":
			var b gomp4.Tkhd
			if err := unmarshal(payload, &b); err != nil {
				return &CorruptError{h.offset, "tkhd: " + err.Error()}
			}
			cur = &TrackInfo{TrackID: b.TrackID}
			init.Tracks[b.TrackID] = cur
		case "mdhd":
			var b gomp4.Mdhd
			if err := unmarshal(payload, &b); err != nil {
				return &CorruptError{h.offset, "mdhd: " + err.Error()}
			}
			if cur != nil {
				cur.Timescale = b.Timescale
			}
		case "trex":
			var b gomp4.Trex
			if err := unmarshal(payload, &b); err != nil {
				return &CorruptError{h.offset, "trex: " + err.Error()}
			}
			t := init.Tracks[b.TrackID]
			if t == nil {
				t = &TrackInfo{TrackID: b.TrackID}
				init.Tracks[b.TrackID] = t
			}
			t.DefaultSampleDuration = b.DefaultSampleDuration
			t.DefaultSampleSize = b.DefaultSampleSize
			t.DefaultSampleFlags = b.DefaultSampleFlags
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(init.Tracks) == 0 {
		return nil, fmt.Errorf("%w: no tracks in init segment", ErrMissingBox)
	}
	return init, nil
}

func tfdtValue(t *gomp4.Tfdt) int64 {
	if t.Version == 1 {
		return int64(t.BaseMediaDecodeTimeV1)
	}
	return int64(t.BaseMediaDecodeTimeV0)
}

// ParseFragment parses one moof+mdat fragment. Sample-level defaults fall
// back from trun to tfhd to trex (via init). Sample ranges are validated
// against mdat; violations return a *CorruptError at the trun offset.
func ParseFragment(data []byte, init *InitInfo) (*Fragment, error) {
	f := &Fragment{Raw: data}
	var mdat []byte
	var truns []*gomp4.Trun
	var trunOffs []int64
	var tfhd *gomp4.Tfhd
	var tfdt *gomp4.Tfdt
	haveMfhd := false
	err := walk(data, func(h boxHeader, payload []byte) error {
		f.Boxes = append(f.Boxes, BoxInfo{h.typ, h.offset, h.size})
		raw := payload // go-mp4 marshals payloads only
		switch h.typ {
		case "mfhd":
			var b gomp4.Mfhd
			if err := unmarshal(payload, &b); err != nil {
				return &CorruptError{h.offset, "mfhd: " + err.Error()}
			}
			f.Seq, haveMfhd = b.SequenceNumber, true
			f.Parsed = append(f.Parsed, ParsedBox{"mfhd", h.offset, &b, raw})
		case "tfhd":
			var b gomp4.Tfhd
			if err := unmarshal(payload, &b); err != nil {
				return &CorruptError{h.offset, "tfhd: " + err.Error()}
			}
			tfhd = &b
			f.Parsed = append(f.Parsed, ParsedBox{"tfhd", h.offset, &b, raw})
		case "tfdt":
			var b gomp4.Tfdt
			if err := unmarshal(payload, &b); err != nil {
				return &CorruptError{h.offset, "tfdt: " + err.Error()}
			}
			tfdt = &b
			f.Parsed = append(f.Parsed, ParsedBox{"tfdt", h.offset, &b, raw})
		case "trun":
			var b gomp4.Trun
			if err := unmarshal(payload, &b); err != nil {
				return &CorruptError{h.offset, "trun: " + err.Error()}
			}
			truns, trunOffs = append(truns, &b), append(trunOffs, h.offset)
			f.Parsed = append(f.Parsed, ParsedBox{"trun", h.offset, &b, raw})
		case "mdat":
			mdat = payload
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, req := range []struct {
		ok   bool
		name string
	}{{haveMfhd, "mfhd"}, {tfhd != nil, "tfhd"}, {tfdt != nil, "tfdt"}, {len(truns) > 0, "trun"}, {mdat != nil, "mdat"}} {
		if !req.ok {
			return nil, fmt.Errorf("%w: %s", ErrMissingBox, req.name)
		}
	}
	f.TrackID = tfhd.TrackID
	f.TFDT = tfdtValue(tfdt)
	track, ok := init.Track(f.TrackID)
	if !ok {
		return nil, fmt.Errorf("unknown track id %d", f.TrackID)
	}
	tf := tfhd.GetFlags()
	defDur, defSize, defFlags := track.DefaultSampleDuration, track.DefaultSampleSize, track.DefaultSampleFlags
	if tf&gomp4.TfhdDefaultSampleDurationPresent != 0 {
		defDur = tfhd.DefaultSampleDuration
	}
	if tf&gomp4.TfhdDefaultSampleSizePresent != 0 {
		defSize = tfhd.DefaultSampleSize
	}
	if tf&gomp4.TfhdDefaultSampleFlagsPresent != 0 {
		defFlags = tfhd.DefaultSampleFlags
	}
	dts, mOff := f.TFDT, 0
	for ti, trun := range truns {
		fl := trun.GetFlags()
		for _, e := range trun.Entries {
			dur, size, sflags := defDur, defSize, defFlags
			if fl&0x100 != 0 {
				dur = e.SampleDuration
			}
			if fl&0x200 != 0 {
				size = e.SampleSize
			}
			if fl&0x400 != 0 {
				sflags = e.SampleFlags
			}
			var cto int64
			if fl&0x800 != 0 {
				if trun.Version == 1 {
					cto = int64(e.SampleCompositionTimeOffsetV1)
				} else {
					cto = int64(e.SampleCompositionTimeOffsetV0)
				}
			}
			if mOff+int(size) > len(mdat) {
				return nil, &CorruptError{trunOffs[ti], fmt.Sprintf("sample %d size %d exceeds mdat (%d bytes left)", len(f.Samples), size, len(mdat)-mOff)}
			}
			f.Samples = append(f.Samples, Sample{
				TrackID: f.TrackID, Seq: f.Seq, Index: len(f.Samples),
				DTS: dts, PTS: dts + cto, Duration: dur, Size: size,
				Keyframe: sflags&0x10000 == 0, Data: mdat[mOff : mOff+int(size)],
			})
			mOff += int(size)
			dts += int64(dur)
		}
	}
	return f, nil
}
