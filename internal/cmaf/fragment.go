package cmaf

import (
	"fmt"

	"github.com/abema/go-mp4"

	"mediaflow/internal/clock"
)

func (s *Scanner) parseMoof(buf []byte, moof Box, segIdx int) Fragment {
	frag := Fragment{Segment: segIdx, MoofOffset: uint64(moof.Offset), TfdtTicks: make(map[uint32]uint64)}
	kids, probs := children(buf, moof)
	for _, p := range probs {
		frag.Problems = append(frag.Problems, problemToLocated(p, segIdx))
	}
	if mfhdBox, ok := findBox(kids, "mfhd"); ok {
		var mfhd mp4.Mfhd
		if err := decodeBox(buf, mfhdBox, &mfhd); err != nil {
			frag.Problems = append(frag.Problems, clock.LocatedError{
				Kind: clock.ErrBoxTruncated, Segment: segIdx, Offset: uint64(mfhdBox.Offset), Detail: err.Error(),
			})
		} else {
			frag.SequenceNumber = mfhd.SequenceNumber
			frag.HasMfhd = true
		}
	} else {
		frag.Problems = append(frag.Problems, clock.LocatedError{
			Kind: clock.ErrBoxTruncated, Segment: segIdx, Offset: uint64(moof.Offset), Detail: "moof without mfhd",
		})
	}

	for _, trafBox := range kids {
		if trafBox.Type != "traf" {
			continue
		}
		s.parseTraf(buf, trafBox, moof, &frag, segIdx)
	}
	if len(frag.Samples) > s.Limits.MaxSamplesPerFragment {
		frag.Problems = append(frag.Problems, clock.LocatedError{
			Kind: clock.ErrSampleOutOfBounds, Segment: segIdx, Offset: uint64(moof.Offset),
			Detail: fmt.Sprintf("fragment has %d samples, limit %d", len(frag.Samples), s.Limits.MaxSamplesPerFragment),
		})
	}
	return frag
}

func (s *Scanner) parseTraf(buf []byte, trafBox, moof Box, frag *Fragment, segIdx int) {
	kids, probs := children(buf, trafBox)
	for _, p := range probs {
		frag.Problems = append(frag.Problems, problemToLocated(p, segIdx))
	}

	var tfhd mp4.Tfhd
	haveTfhd := false
	if b, ok := findBox(kids, "tfhd"); ok {
		if err := decodeBox(buf, b, &tfhd); err == nil {
			haveTfhd = true
		} else {
			frag.Problems = append(frag.Problems, clock.LocatedError{
				Kind: clock.ErrBoxTruncated, Segment: segIdx, Offset: uint64(b.Offset), Detail: err.Error(),
			})
		}
	}
	if !haveTfhd {
		return
	}

	var base uint64
	baseIsMoof := true
	if tfhd.CheckFlag(mp4.TfhdBaseDataOffsetPresent) {
		base = tfhd.BaseDataOffset
		baseIsMoof = false
	}
	if baseIsMoof {
		base = uint64(moof.Offset)
	}

	var baseTicks uint64
	timeKnown := true
	if b, ok := findBox(kids, "tfdt"); ok {
		var tfdt mp4.Tfdt
		if err := decodeBox(buf, b, &tfdt); err != nil {
			frag.Problems = append(frag.Problems, clock.LocatedError{
				Kind: clock.ErrBoxTruncated, Segment: segIdx, Offset: uint64(b.Offset), Detail: err.Error(),
			})
			timeKnown = false
		} else {
			baseTicks = tfdt.GetBaseMediaDecodeTime()
			frag.HasTfdt = true
			frag.TfdtTicks[tfhd.TrackID] = baseTicks
		}
	} else {
		timeKnown = false
		frag.Problems = append(frag.Problems, clock.LocatedError{
			Kind: clock.ErrBoxTruncated, Segment: segIdx, Offset: uint64(trafBox.Offset), Detail: "traf without tfdt",
		})
	}

	trex := s.Defaults[tfhd.TrackID]
	sampleDur := tfhd.DefaultSampleDuration
	if sampleDur == 0 {
		sampleDur = trex.DefaultSampleDuration
	}
	sampleSize := tfhd.DefaultSampleSize
	if sampleSize == 0 {
		sampleSize = trex.DefaultSampleSize
	}
	sampleFlags := tfhd.DefaultSampleFlags
	if sampleFlags == 0 {
		sampleFlags = trex.DefaultSampleFlags
	}

	// A traf can carry several truns; each data offset is relative to base.
	nextOffset := base
	index := 0
	for _, trunBox := range kids {
		if trunBox.Type != "trun" {
			continue
		}
		var trun mp4.Trun
		if err := decodeBox(buf, trunBox, &trun); err != nil {
			frag.Problems = append(frag.Problems, clock.LocatedError{
				Kind: clock.ErrBoxTruncated, Segment: segIdx, Offset: uint64(trunBox.Offset), Detail: err.Error(),
			})
			continue
		}
		if trun.CheckFlag(0x000001) {
			nextOffset = base + uint64(int64(trun.DataOffset))
		}
		if trun.SampleCount > uint32(s.Limits.MaxSamplesPerFragment) {
			frag.Problems = append(frag.Problems, clock.LocatedError{
				Kind: clock.ErrSampleOutOfBounds, Segment: segIdx, Offset: uint64(trunBox.Offset),
				Detail: fmt.Sprintf("trun declares %d samples, limit %d", trun.SampleCount, s.Limits.MaxSamplesPerFragment),
			})
			continue
		}
		curTicks := baseTicks
		for i := 0; i < int(trun.SampleCount); i++ {
			var entry mp4.TrunEntry
			if i < len(trun.Entries) {
				entry = trun.Entries[i]
			}
			dur := sampleDur
			if trun.CheckFlag(0x000100) {
				dur = entry.SampleDuration
			}
			sz := sampleSize
			if trun.CheckFlag(0x000200) {
				sz = entry.SampleSize
			}
			flags := sampleFlags
			if i == 0 && trun.CheckFlag(0x000004) {
				flags = trun.FirstSampleFlags
			} else if trun.CheckFlag(0x000400) {
				flags = entry.SampleFlags
			}
			nonSync := flags&0x00010000 != 0
			sample := Sample{
				TrackID:         tfhd.TrackID,
				Index:           index,
				Segment:         segIdx,
				Offset:          nextOffset,
				Size:            sz,
				DurationTicks:   dur,
				DecodeTimeTicks: curTicks,
				IsKeyframe:      !nonSync,
				NonSync:         nonSync,
				TimeKnown:       timeKnown,
				Flags:           flags,
			}
			frag.Samples = append(frag.Samples, sample)
			curTicks += uint64(dur)
			nextOffset += uint64(sz)
			index++
		}
	}
}
