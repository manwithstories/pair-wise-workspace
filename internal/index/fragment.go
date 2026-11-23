package index

import (
	"fmt"

	"github.com/abema/go-mp4"

	"mediascan/internal/clock"
)

// fragmentState tracks the byte position of the previous traf so that a trun
// without an explicit base data offset can still be resolved.
type fragmentState struct {
	prevEnd  int64
	havePrev bool
}

// parseMoof reads one movie fragment and appends its samples to the matching
// track. Timestamps come from tfdt (version 1 in generated media), which is the
// authoritative decode time for a fragment.
func (s *source) parseMoof(mf *mp4.BoxInfo) error {
	trafs, err := s.extractBoxes(mf, mp4.BoxTypeTraf())
	if err != nil {
		return fmt.Errorf("%w: traf: %v", ErrUnsupported, err)
	}
	st := fragmentState{}
	for _, traf := range trafs {
		if err := s.parseTraf(mf, traf, &st); err != nil {
			return err
		}
	}
	return nil
}

func (s *source) parseTraf(mf, traf *mp4.BoxInfo, st *fragmentState) error {
	tfhdBox, err := s.payload(traf, mp4.BoxTypeTfhd())
	if err != nil {
		return fmt.Errorf("%w: tfhd: %v", ErrUnsupported, err)
	}
	tfhd, ok := tfhdBox.(*mp4.Tfhd)
	if !ok || tfhd == nil {
		return fmt.Errorf("%w: traf without tfhd", ErrUnsupported)
	}
	ts := s.trackByID(tfhd.TrackID)
	if ts == nil {
		return fmt.Errorf("%w: traf for unknown track %d", ErrUnsupported, tfhd.TrackID)
	}
	tfdtBox, err := s.payload(traf, mp4.BoxTypeTfdt())
	if err != nil {
		return fmt.Errorf("%w: tfdt: %v", ErrUnsupported, err)
	}
	var base uint64
	if tfdt, ok := tfdtBox.(*mp4.Tfdt); ok && tfdt != nil {
		base = tfdt.GetBaseMediaDecodeTime()
	}

	// Resolve the base data offset for this traf's runs.
	baseOff := int64(mf.Offset)
	if tfhd.CheckFlag(mp4.TfhdBaseDataOffsetPresent) {
		baseOff = int64(tfhd.BaseDataOffset)
	} else if !tfhd.CheckFlag(mp4.TfhdDefaultBaseIsMoof) && st.havePrev {
		baseOff = st.prevEnd
	}

	bips, err := mp4.ExtractBoxesWithPayload(s.file, traf, []mp4.BoxPath{{mp4.BoxTypeTrun()}})
	if err != nil {
		return fmt.Errorf("%w: trun: %v", ErrUnsupported, err)
	}
	if len(bips) == 0 {
		return fmt.Errorf("%w: traf without trun", ErrUnsupported)
	}
	var decode uint64 = base
	trafEnd := baseOff
	for _, bip := range bips {
		trun, ok := bip.Payload.(*mp4.Trun)
		if !ok || trun == nil {
			return fmt.Errorf("%w: bad trun payload", ErrUnsupported)
		}
		flags := trun.GetFlags()
		cursor := baseOff + int64(trun.DataOffset)
		defDur := int64(tfhd.DefaultSampleDuration)
		defSize := int64(tfhd.DefaultSampleSize)
		for i := range trun.Entries {
			e := trun.Entries[i]
			dur := defDur
			if flags&0x100 != 0 {
				dur = int64(e.SampleDuration)
			}
			size := defSize
			if flags&0x200 != 0 {
				size = int64(e.SampleSize)
			}
			sampleFlags := uint32(0)
			if flags&0x400 != 0 {
				sampleFlags = e.SampleFlags
			} else if i == 0 && flags&0x004 != 0 {
				sampleFlags = trun.FirstSampleFlags
			}
			if size <= 0 || dur <= 0 {
				return fmt.Errorf("%w: trun sample with non-positive size/duration", ErrTruncated)
			}
			end := cursor + size
			if cursor < 0 || end > s.size {
				s.truncated = true
				return fmt.Errorf("%w: sample at %d+%d past EOF %d", ErrTruncated, cursor, size, s.size)
			}
			timeNs := clock.ToNanos(decode, ts.timescale)
			ts.samples = append(ts.samples, sampleRef{
				offset: cursor,
				size:   size,
				timeNs: timeNs,
				durNs:  clock.NanosDelta(dur, ts.timescale),
				sync:   sampleFlags&0x00010000 == 0,
			})
			decode += uint64(dur)
			cursor = end
		}
		if cursor > trafEnd {
			trafEnd = cursor
		}
	}
	st.prevEnd = trafEnd
	st.havePrev = true
	return nil
}

func (s *source) trackByID(id uint32) *trackSource {
	for i := range s.tracks {
		if s.tracks[i].id == id {
			return &s.tracks[i]
		}
	}
	return nil
}
