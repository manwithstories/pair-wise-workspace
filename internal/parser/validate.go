package parser

import (
	"math"

	"mediascan/internal/media"
)

func (f *File) addFault(code, severity string, trackID uint32, offset int64, t media.Time, format string, args ...any) {
	f.Faults = append(f.Faults, media.NewFault(code, severity, f.Source.SHA256, f.Source.Name, trackID, offset, t, format, args...))
}

func validateSampleRanges(f *File) {
	if f.Source == nil {
		return
	}
	fileSize := uint64(f.Source.Size)
	for _, track := range f.Tracks {
		samplePos := 0
		for chunkIndex, entry := range track.ChunkOffsets {
			spc := samplesForChunk(track.stscEntries, uint32(chunkIndex+1))
			first := samplePos
			chunkOffset := entry.Value

			if chunkOffset > fileSize {
				t := sampleMediaTime(track, first)
				f.addFault(media.CodeSampleOffsetOOB, media.SeverityError, track.TrackID, entry.ValueOffset, t,
					"chunk_offset[%d]=%d is beyond file size %d", chunkIndex, chunkOffset, fileSize)
				skipChunkSamples(track, &samplePos, spc)
				continue
			}

			bad := false
			for j := uint32(0); j < spc && samplePos < len(track.Samples); j++ {
				s := &track.Samples[samplePos]
				size := s.Size
				if size > math.MaxUint64-chunkOffset {
					t := sampleMediaTime(track, samplePos)
					f.addFault(media.CodeIntegerOverflow, media.SeverityError, track.TrackID, entry.ValueOffset, t,
						"sample range overflows: offset=%d size=%d", chunkOffset, size)
					bad = true
					break
				}
				end := chunkOffset + size
				if end > fileSize {
					mt := sampleMediaTime(track, samplePos)
					f.addFault(media.CodeSampleOffsetOOB, media.SeverityError, track.TrackID, entry.ValueOffset, mt,
						"sample %d range [%d,%d) exceeds file size %d", s.Number, chunkOffset, end, fileSize)
					bad = true
					break
				}
				if !f.insideMDAT(chunkOffset, end) {
					mt := sampleMediaTime(track, samplePos)
					f.addFault(media.CodeSampleOutsideMDAT, media.SeverityError, track.TrackID, entry.ValueOffset, mt,
						"sample %d range [%d,%d) is outside all mdat boxes", s.Number, chunkOffset, end)
				}
				s.Offset = chunkOffset
				chunkOffset = end
				samplePos++
			}
			if bad {
				skipChunkSamples(track, &samplePos, spc)
			}
		}
	}
}

func skipChunkSamples(t *Track, pos *int, spc uint32) {
	for j := uint32(0); j < spc && *pos < len(t.Samples); j++ {
		(*pos)++
	}
}

func sampleMediaTime(t *Track, index int) media.Time {
	if index >= 0 && index < len(t.Samples) {
		return media.NewTime(t.Samples[index].DecodeTime, t.Timescale)
	}
	return media.NewTime(0, t.Timescale)
}

func (f *File) insideMDAT(start, end uint64) bool {
	if len(f.MDAT) == 0 {
		return true // header-only fixtures and malformed mdat are reported separately
	}
	for _, r := range f.MDAT {
		if start >= uint64(r.Offset) && end <= uint64(r.End) {
			return true
		}
	}
	return false
}

func validateFragmentRanges(f *File) {
	if f.Source == nil {
		return
	}
	fileSize := uint64(f.Source.Size)
	for _, track := range f.Tracks {
		var expectedEnd int64
		havePrev := false
		for fi, frag := range track.Fragments {
			base := frag.BaseTime
			if havePrev && base < expectedEnd {
				f.addFault(media.CodeTfdtRegression, media.SeverityError, track.TrackID, frag.TfdtOffset,
					media.NewTime(base, track.Timescale),
					"tfdt[%d]=%d precedes expected decode time %d", fi, base, expectedEnd)
			}

			durationSum := int64(0)
			for _, s := range frag.Samples {
				if int64(s.Duration) > math.MaxInt64-durationSum {
					durationSum = math.MaxInt64
					break
				}
				durationSum += int64(s.Duration)
			}
			if base >= 0 && durationSum <= math.MaxInt64-base {
				expectedEnd = base + durationSum
				havePrev = true
			}

			if frag.TrunOffset == 0 || len(frag.Samples) == 0 {
				continue
			}
			if frag.DataOffset == 0 {
				continue
			}
			if frag.MoofOffset < 0 || int64(frag.DataOffset) < 0 && frag.MoofOffset < -int64(frag.DataOffset) {
				f.addFault(media.CodeIntegerOverflow, media.SeverityError, track.TrackID, frag.TrunOffset,
					media.NewTime(base, track.Timescale), "fragment data offset underflows moof")
				continue
			}
			dataStart := frag.MoofOffset + int64(frag.DataOffset)
			pos := uint64(dataStart)
			decode := base
			for _, s := range frag.Samples {
				start := pos
				size := uint64(s.Size)
				if size > math.MaxUint64-start {
					f.addFault(media.CodeIntegerOverflow, media.SeverityError, track.TrackID, frag.TrunOffset,
						media.NewTime(decode, track.Timescale), "trun sample range overflows")
					break
				}
				end := start + size
				if start > fileSize || end > fileSize {
					f.addFault(media.CodeSampleOffsetOOB, media.SeverityError, track.TrackID, frag.TrunOffset,
						media.NewTime(decode, track.Timescale), "fragment sample %d range [%d,%d) exceeds EOF %d", s.Number, start, end, fileSize)
					break
				}
				if !f.insideMDAT(start, end) {
					f.addFault(media.CodeSampleOutsideMDAT, media.SeverityError, track.TrackID, frag.TrunOffset,
						media.NewTime(decode, track.Timescale), "fragment sample %d outside mdat", s.Number)
				}
				pos = end
				if decode < math.MaxInt64-int64(s.Duration) {
					decode += int64(s.Duration)
				}
			}
		}
	}
}
