package demux

import (
	"fmt"

	"bcast/media"

	mp4 "github.com/abema/go-mp4"
)

// buildAllSamples expands every raw track's sample table into media.Sample
// entries. Samples whose payload falls outside the file (truncated tail)
// are dropped and reported as a damaged range, keeping earlier samples.
func buildAllSamples(f *File, fileSize int64) error {
	var firstErr error
	for _, rt := range f.raw {
		if err := buildTrackSamples(f, rt, fileSize); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func buildTrackSamples(f *File, rt *rawTrack, fileSize int64) error {
	if rt.stts == nil || rt.stsz == nil || rt.stsc == nil || (rt.stco == nil && rt.co64 == nil) {
		return media.Recoverable("demux.index", -1,
			fmt.Errorf("%w: track %d missing stts/stsz/stsc/stco", media.ErrIncompleteIndex, rt.id))
	}
	deltas := expandStts(rt.stts)
	sizes := sampleSizes(rt)
	if len(deltas) != len(sizes) {
		return media.Recoverable("demux.index", -1,
			fmt.Errorf("%w: track %d stts=%d stsz=%d", media.ErrIncompleteIndex, rt.id, len(deltas), len(sizes)))
	}
	ctsOff := cttsOffsets(rt, len(deltas))
	chunkOffs := chunkOffsets(rt)
	key := stssSet(rt)

	ts := media.Time{Timescale: rt.timescale}
	samples := make([]media.Sample, 0, len(deltas))
	idx := 0 // global sample index
	var dts int64
	truncAt := int64(-1)
	for ci, coff := range chunkOffs {
		perChunk := samplesPerChunk(rt, uint32(ci+1))
		off := coff
		for j := uint32(0); j < perChunk && idx < len(sizes); j++ {
			if truncAt >= 0 {
				break
			}
			sz := sizes[idx]
			if off+int64(sz) > fileSize {
				truncAt = off
				break
			}
			s := media.Sample{
				TrackID:  rt.id,
				Kind:     rt.kind,
				DTS:      media.Time{Value: dts, Timescale: ts.Timescale},
				CTS:      media.Time{Value: dts + int64(ctsOff[idx]), Timescale: ts.Timescale},
				Duration: deltas[idx],
				Offset:   off,
				Size:     sz,
			}
			if rt.kind == media.TrackVideo {
				if rt.stss != nil {
					s.Keyframe = key[idx+1]
					s.KeySource = media.KeySrcSTSS
				} else {
					s.KeySource = media.KeySrcNone // rebuilt later by index.go
				}
			}
			samples = append(samples, s)
			dts += int64(deltas[idx])
			off += int64(sz)
			idx++
		}
	}
	if truncAt >= 0 {
		f.Damaged = append(f.Damaged, media.Range{
			Start: truncAt, End: fileSize,
			Reason: fmt.Sprintf("track %d: %s", rt.id, media.ErrTruncated),
		})
		f.Samples = append(f.Samples, samples...)
		return media.Recoverable("demux.samples", truncAt, media.ErrTruncated)
	}
	if idx < len(sizes) {
		return media.Recoverable("demux.index", -1,
			fmt.Errorf("%w: track %d chunks cover %d of %d samples", media.ErrIncompleteIndex, rt.id, idx, len(sizes)))
	}
	f.Samples = append(f.Samples, samples...)
	return nil
}

// expandStts flattens (count, delta) decoding-time entries.
func expandStts(stts *mp4.Stts) []uint32 {
	var out []uint32
	for _, e := range stts.Entries {
		for i := uint32(0); i < e.SampleCount; i++ {
			out = append(out, e.SampleDelta)
		}
	}
	return out
}

func sampleSizes(rt *rawTrack) []uint32 {
	if rt.stsz.SampleSize != 0 {
		out := make([]uint32, rt.stsz.SampleCount)
		for i := range out {
			out[i] = rt.stsz.SampleSize
		}
		return out
	}
	return rt.stsz.EntrySize
}

func cttsOffsets(rt *rawTrack, n int) []uint32 {
	out := make([]uint32, n)
	if rt.ctts == nil {
		return out
	}
	i := 0
	for _, e := range rt.ctts.Entries {
		for k := uint32(0); k < e.SampleCount && i < n; k++ {
			out[i] = e.SampleOffsetV0
			i++
		}
	}
	return out
}

func chunkOffsets(rt *rawTrack) []int64 {
	if rt.co64 != nil {
		out := make([]int64, len(rt.co64.ChunkOffset))
		for i, o := range rt.co64.ChunkOffset {
			out[i] = int64(o)
		}
		return out
	}
	out := make([]int64, len(rt.stco.ChunkOffset))
	for i, o := range rt.stco.ChunkOffset {
		out[i] = int64(o)
	}
	return out
}

// samplesPerChunk resolves the stsc table for 1-based chunk number c.
func samplesPerChunk(rt *rawTrack, c uint32) uint32 {
	n := uint32(0)
	for _, e := range rt.stsc.Entries {
		if e.FirstChunk <= c {
			n = e.SamplesPerChunk
		} else {
			break
		}
	}
	return n
}

func stssSet(rt *rawTrack) map[int]bool {
	m := map[int]bool{}
	if rt.stss != nil {
		for _, n := range rt.stss.SampleNumber {
			m[int(n)] = true
		}
	}
	return m
}
