package mp4

import (
	"fmt"
	"io"
	"sort"

	mp4 "github.com/abema/go-mp4"
)

// zeroRunDamage is the byte run length at which a zero-filled region is
// treated as damage rather than a legitimate low-entropy payload.
const zeroRunDamage = 512

// corruptionProbeBytes caps how much of one sample is read for the zero-run
// check. A malicious stsz can declare multi-gigabyte samples; the scan must
// stay well inside the 512 MiB budget regardless.
const corruptionProbeBytes = 8192

// flagCorruption marks samples whose bytes fall outside the file or contain a
// long zero run. Fixture payloads are pseudo-random, so a run of zeroRunDamage
// zero bytes (or half a small sample) is damage rather than media.
func flagCorruption(r io.ReaderAt, tr *Track, size int64) {
	for i := range tr.Samples {
		s := &tr.Samples[i]
		if s.Size == 0 {
			continue
		}
		if s.Offset < 0 || s.Offset+int64(s.Size) > size {
			s.Corrupt = true
			continue
		}
		probe := int64(s.Size)
		if probe > corruptionProbeBytes {
			probe = corruptionProbeBytes
		}
		buf := make([]byte, probe)
		if _, err := r.ReadAt(buf, s.Offset); err != nil {
			s.Corrupt = true
			continue
		}
		limit := zeroRunDamage
		if half := len(buf) / 2; half < limit {
			limit = half
		}
		if _, run := zeroRun(buf); run > 0 && run >= limit {
			s.Corrupt = true
		}
	}
}

func zeroRun(buf []byte) (start, length int) {
	bestStart, best, cur := 0, 0, 0
	for i, b := range buf {
		if b == 0 {
			cur++
			if cur == 1 {
				start = i
			}
			if cur > best {
				bestStart, best = start, cur
			}
			continue
		}
		cur = 0
	}
	return bestStart, best
}

func constantDurations(stts mp4.Stts) bool {
	seen := map[uint32]bool{}
	for _, e := range stts.Entries {
		if e.SampleCount > 0 {
			seen[e.SampleDelta] = true
		}
	}
	return len(seen) <= 1
}

func keyframeGaps(samples []Sample) []int64 {
	var gaps []int64
	var last int64 = -1
	for _, s := range samples {
		if !s.Keyframe || s.Isolated {
			continue
		}
		if last >= 0 {
			gaps = append(gaps, s.DTS-last)
		}
		last = s.DTS
	}
	return gaps
}

// quantile returns the nearest-rank percentile of an unsorted set.
func quantile(xs []int64, p int) int64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]int64(nil), xs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := (p*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// maxSamplesPerTrack bounds how many sample entries are materialized for one
// track. At 48 bytes each, 2M samples is about 96 MiB of index state plus the
// parallel size/duration slices, which keeps a hostile stsz from exhausting the
// 512 MiB budget. A timeline needing more than this is refused with a fault.
const maxSamplesPerTrack = 2_000_000

// sampleSizes expands a constant-size stsz into per-sample sizes. The declared
// count is checked before allocation so a hostile header cannot force a huge
// allocation; exceeding the cap is a located error, not a silent truncation.
func sampleSizes(stsz mp4.Stsz) ([]uint32, error) {
	if stsz.SampleSize != 0 {
		if int64(stsz.SampleCount) > maxSamplesPerTrack {
			return nil, fmt.Errorf("stsz declares %d samples, above the %d sample limit", stsz.SampleCount, maxSamplesPerTrack)
		}
		out := make([]uint32, stsz.SampleCount)
		for i := range out {
			out[i] = stsz.SampleSize
		}
		return out, nil
	}
	if len(stsz.EntrySize) > maxSamplesPerTrack {
		return nil, fmt.Errorf("stsz declares %d samples, above the %d sample limit", len(stsz.EntrySize), maxSamplesPerTrack)
	}
	return stsz.EntrySize, nil
}

func chunkOffsets(r io.ReaderAt, stcoBox, co64Box *box) ([]uint64, error) {
	if stcoBox != nil {
		var stco mp4.Stco
		if err := unmarshal(r, *stcoBox, &stco); err != nil {
			return nil, fmt.Errorf("stco: %w", err)
		}
		out := make([]uint64, len(stco.ChunkOffset))
		for i, v := range stco.ChunkOffset {
			out[i] = uint64(v)
		}
		return out, nil
	}
	if co64Box != nil {
		var co64 mp4.Co64
		if err := unmarshal(r, *co64Box, &co64); err != nil {
			return nil, fmt.Errorf("co64: %w", err)
		}
		return co64.ChunkOffset, nil
	}
	return nil, fmt.Errorf("no chunk offset box")
}

// buildSamples reconstructs per-sample offset, decode time and duration. A
// sample whose duration entry is missing is isolated rather than guessed.
func buildSamples(sizes []uint32, stts mp4.Stts, stsc mp4.Stsc, chunkOff []uint64, keyframes map[uint32]bool, hasKeyframes bool) []Sample {
	n := len(sizes)
	durs := make([]int64, n)
	var idx int
	for _, e := range stts.Entries {
		for i := uint32(0); i < e.SampleCount && idx < n; i++ {
			durs[idx] = int64(e.SampleDelta)
			idx++
		}
	}
	offs := make([]int64, n)
	si := 0
	for ci := 0; ci < len(chunkOff) && si < n; ci++ {
		per := samplesPerChunk(stsc, uint32(ci+1))
		off := int64(chunkOff[ci])
		for k := uint32(0); k < per && si < n; k++ {
			offs[si] = off
			off += int64(sizes[si])
			si++
		}
	}
	out := make([]Sample, n)
	var dts int64
	for i := 0; i < n; i++ {
		s := Sample{
			Index:    uint32(i + 1),
			Offset:   offs[i],
			Size:     sizes[i],
			DTS:      dts,
			Duration: durs[i],
			Keyframe: !hasKeyframes || keyframes[uint32(i+1)],
		}
		if s.Duration <= 0 {
			s.Isolated = true
		} else {
			dts += s.Duration
		}
		out[i] = s
	}
	return out
}

func samplesPerChunk(stsc mp4.Stsc, chunk uint32) uint32 {
	per := uint32(1)
	for _, e := range stsc.Entries {
		if e.FirstChunk <= chunk {
			per = e.SamplesPerChunk
		}
	}
	return per
}

func sampleEntryCodec(r io.ReaderAt, stsd box) string {
	// stsd is a FullBox (4) plus entry_count (4), then the first sample entry.
	hdr := make([]byte, 16)
	if _, err := r.ReadAt(hdr, int64(stsd.offset+stsd.header+8)); err != nil {
		return ""
	}
	return string(hdr[4:8])
}

func unmarshal(r io.ReaderAt, b box, dst mp4.IBox) error {
	if b.size < b.header {
		return fmt.Errorf("box %s truncated", b.typ)
	}
	payload := io.NewSectionReader(r, int64(b.offset+b.header), int64(b.size-b.header))
	_, err := mp4.Unmarshal(payload, b.size-b.header, dst, mp4.Context{})
	return err
}

func selectBoxes(all []box, path string) []box {
	out := make([]box, 0, 1)
	for _, b := range all {
		if strPath(b.path) == path {
			out = append(out, b)
		}
	}
	return out
}

func strPath(p []mp4.BoxType) string {
	s := ""
	for i, t := range p {
		if i > 0 {
			s += "/"
		}
		s += t.String()
	}
	return s
}
