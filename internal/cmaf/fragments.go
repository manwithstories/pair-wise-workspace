package cmaf

import (
	"fmt"
)

// Full-box flag bits defined by ISO 14496-12.
const (
	tfhdDefaultSampleDuration = 0x000008
	tfhdDefaultSampleSize     = 0x000010

	trunDataOffset       = 0x000001
	trunFirstSampleFlags = 0x000004
	trunSampleDuration   = 0x000100
	trunSampleSize       = 0x000200
	trunSampleFlags      = 0x000400
	trunCompositionTime  = 0x000800
)

// Sample is one trun entry. Missing optional fields stay zero; callers fall
// back to the track's tfhd defaults when Duration/Size are zero.
type Sample struct {
	Duration          uint32
	Size              uint32
	Flags             uint32
	CompositionOffset int64
}

// TrackFragment is the parsed traf box of one track.
type TrackFragment struct {
	TrackID               uint32
	BaseMediaDecodeTime   uint64
	DefaultSampleDuration uint32
	DefaultSampleSize     uint32
	DataOffset            int32
	HasDataOffset         bool
	TrunVersion           byte
	Samples               []Sample
}

// Fragment is one moof/mdat pair (a CMAF fragment group).
type Fragment struct {
	SequenceNumber uint32
	Moof           Box
	Mdat           Box
	Tracks         []TrackFragment
	Start          int64
	End            int64
}

// ParseFragmentStream parses concatenated top-level boxes into fragments.
// Duplicate mfhd sequence numbers are hard-rejected (the offset points at
// the offending mfhd); gaps in the sequence are returned for the recovery
// layer instead of being silently filled.
func ParseFragmentStream(data []byte) (frags []*Fragment, gaps []uint32, err error) {
	boxes, err := ScanBoxes(data, 0)
	if err != nil {
		return nil, nil, err
	}
	seen := map[uint32]int64{}
	var lastSeq uint32
	haveLast := false
	for i := 0; i < len(boxes); i++ {
		if boxes[i].Type != TypeMoof {
			return nil, nil, &BoxError{Offset: boxes[i].Start, BoxType: boxes[i].Type,
				Reason: "expected moof at top level"}
		}
		f, perr := parseMoof(boxes[i])
		if perr != nil {
			return nil, nil, perr
		}
		if off, dup := seen[f.SequenceNumber]; dup {
			return nil, nil, &BoxError{Offset: off, BoxType: TypeMfhd,
				Reason: fmt.Sprintf("conflicting sequence number %d", f.SequenceNumber)}
		}
		seen[f.SequenceNumber] = f.Moof.Start
		if haveLast && uint64(f.SequenceNumber) > uint64(lastSeq)+1 {
			for s := uint64(lastSeq) + 1; s < uint64(f.SequenceNumber); s++ {
				gaps = append(gaps, uint32(s))
			}
		}
		lastSeq, haveLast = f.SequenceNumber, true
		if i+1 >= len(boxes) || boxes[i+1].Type != TypeMdat {
			return nil, nil, &BoxError{Offset: f.Moof.End(), BoxType: TypeMdat,
				Reason: "moof without following mdat"}
		}
		f.Mdat = boxes[i+1]
		f.Start, f.End = f.Moof.Start, f.Mdat.End()
		if verr := validateSampleRanges(f); verr != nil {
			return nil, nil, verr
		}
		frags = append(frags, f)
		i++
	}
	return frags, gaps, nil
}

func parseMoof(moof Box) (*Fragment, error) {
	kids, err := ChildBoxes(moof, moof.Start)
	if err != nil {
		return nil, err
	}
	f := &Fragment{Moof: moof}
	mfhd, ok := Find(kids, TypeMfhd)
	if !ok {
		return nil, &BoxError{Offset: moof.Start, BoxType: TypeMoof, Reason: "missing mfhd"}
	}
	if f.SequenceNumber, err = parseMfhd(mfhd); err != nil {
		return nil, err
	}
	for _, kb := range kids {
		if kb.Type != TypeTraf {
			continue
		}
		tf, terr := parseTraf(kb)
		if terr != nil {
			return nil, terr
		}
		f.Tracks = append(f.Tracks, tf)
	}
	if len(f.Tracks) == 0 {
		return nil, &BoxError{Offset: moof.Start, BoxType: TypeTraf, Reason: "moof has no traf"}
	}
	return f, nil
}

func parseMfhd(b Box) (uint32, error) {
	abs := b.Start + int64(b.HeaderSize)
	r := newReader(b.Data, abs)
	if _, _, err := r.fullBox(TypeMfhd); err != nil {
		return 0, err
	}
	seq, err := r.u32()
	if err != nil {
		return 0, err
	}
	return seq, nil
}

func parseTraf(traf Box) (TrackFragment, error) {
	kids, err := ChildBoxes(traf, traf.Start)
	if err != nil {
		return TrackFragment{}, err
	}
	var tf TrackFragment
	if b, ok := Find(kids, TypeTfhd); ok {
		if err := parseTfhd(b, &tf); err != nil {
			return TrackFragment{}, err
		}
	} else {
		return TrackFragment{}, &BoxError{Offset: traf.Start, BoxType: TypeTraf, Reason: "missing tfhd"}
	}
	if b, ok := Find(kids, TypeTfdt); ok {
		if tf.BaseMediaDecodeTime, err = parseTfdt(b); err != nil {
			return TrackFragment{}, err
		}
	}
	if b, ok := Find(kids, TypeTrun); ok {
		if err := parseTrun(b, &tf); err != nil {
			return TrackFragment{}, err
		}
	}
	return tf, nil
}

func parseTfhd(b Box, tf *TrackFragment) error {
	r := newReader(b.Data, b.Start+int64(b.HeaderSize))
	_, flags, err := r.fullBox(TypeTfhd)
	if err != nil {
		return err
	}
	if tf.TrackID, err = r.u32(); err != nil {
		return err
	}
	if flags&0x01 != 0 { // base-data-offset
		if _, err = r.u64(); err != nil {
			return err
		}
	}
	if flags&0x02 != 0 { // sample-description-index
		if _, err = r.u32(); err != nil {
			return err
		}
	}
	if flags&tfhdDefaultSampleDuration != 0 {
		if tf.DefaultSampleDuration, err = r.u32(); err != nil {
			return err
		}
	}
	if flags&tfhdDefaultSampleSize != 0 {
		if tf.DefaultSampleSize, err = r.u32(); err != nil {
			return err
		}
	}
	return nil
}

func parseTfdt(b Box) (uint64, error) {
	r := newReader(b.Data, b.Start+int64(b.HeaderSize))
	version, _, err := r.fullBox(TypeTfdt)
	if err != nil {
		return 0, err
	}
	switch version {
	case 0:
		v, err := r.u32()
		return uint64(v), err
	case 1:
		return r.u64()
	default:
		return 0, &BoxError{Offset: b.Start, BoxType: TypeTfdt, Reason: "bad version"}
	}
}

func parseTrun(b Box, tf *TrackFragment) error {
	r := newReader(b.Data, b.Start+int64(b.HeaderSize))
	version, flags, err := r.fullBox(TypeTrun)
	if err != nil {
		return err
	}
	tf.TrunVersion = version
	count, err := r.u32()
	if err != nil {
		return err
	}
	if flags&trunDataOffset != 0 {
		off, err := r.i32()
		if err != nil {
			return err
		}
		tf.DataOffset, tf.HasDataOffset = off, true
	}
	if flags&trunFirstSampleFlags != 0 {
		if _, err = r.u32(); err != nil {
			return err
		}
	}
	tf.Samples = make([]Sample, 0, count)
	for i := uint32(0); i < count; i++ {
		var s Sample
		if flags&trunSampleDuration != 0 {
			if s.Duration, err = r.u32(); err != nil {
				return err
			}
		} else {
			s.Duration = tf.DefaultSampleDuration
		}
		if flags&trunSampleSize != 0 {
			if s.Size, err = r.u32(); err != nil {
				return err
			}
		} else {
			s.Size = tf.DefaultSampleSize
		}
		if flags&trunSampleFlags != 0 {
			if s.Flags, err = r.u32(); err != nil {
				return err
			}
		}
		if flags&trunCompositionTime != 0 {
			// Version 1 offsets are signed; version 0 stays non-negative.
			if s.CompositionOffset, err = r.compositionOffset(version); err != nil {
				return err
			}
		}
		tf.Samples = append(tf.Samples, s)
	}
	return r.err
}

// validateSampleRanges makes sure every declared sample fits inside mdat.
func validateSampleRanges(f *Fragment) error {
	for _, tf := range f.Tracks {
		if !tf.HasDataOffset {
			continue
		}
		pos := int64(tf.DataOffset) // relative to moof start
		mdatLo := f.Mdat.Start - f.Moof.Start + int64(f.Mdat.HeaderSize)
		mdatHi := f.Mdat.End() - f.Moof.Start
		for i, s := range tf.Samples {
			start, end := pos, pos+int64(s.Size)
			if start < mdatLo || end > mdatHi {
				return &BoxError{
					Offset: f.Moof.Start + start, BoxType: TypeTrun,
					Reason: fmt.Sprintf("track %d sample %d outside mdat", tf.TrackID, i),
				}
			}
			pos = end
		}
	}
	return nil
}
