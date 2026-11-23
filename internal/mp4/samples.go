package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"

	"mediascan/internal/clock"
	"mediascan/internal/fault"
)

// Sample describes one track sample positioned on the unified timeline.
type Sample struct {
	Index      int   // zero-based sample index
	IsKeyframe bool  // stss membership (video); audio defaults to true
	Size       int64 // stsz sample size in bytes
	Offset     int64 // chunk byte offset (stco/co64)
	// DurationTicks is the per-sample duration in unified microsecond ticks,
	// computed individually so VFR streams are never averaged.
	DurationTicks int64
	// StartTicks is the cumulative presentation start on the unified grid.
	StartTicks int64
}

// TrackInfo is the fully resolved timing for one trak box.
type TrackInfo struct {
	TrackID   uint32
	Timescale int64
	Samples   []Sample
	Boxes     []*Box // trak root and its key descendants
}

var (
	// ErrMissingTable is returned when a mandatory sample table box is absent.
	ErrMissingTable = errors.New("mp4: missing mandatory sample table box")
	// ErrBadTable is returned for an internally inconsistent sample table.
	ErrBadTable = errors.New("mp4: malformed sample table")
)

// readU32/readU64 are bounds-checked big-endian readers.
func readU32(data []byte, off int64) (uint32, error) {
	if off < 0 || off+4 > int64(len(data)) {
		return 0, fmt.Errorf("%w: u32 read at %d past boundary", ErrTruncated, off)
	}
	return binary.BigEndian.Uint32(data[off : off+4]), nil
}

func readU64(data []byte, off int64) (uint64, error) {
	if off < 0 || off+8 > int64(len(data)) {
		return 0, fmt.Errorf("%w: u64 read at %d past boundary", ErrTruncated, off)
	}
	return binary.BigEndian.Uint64(data[off : off+8]), nil
}

// parseStts expands the time-to-sample box into one duration per sample in
// track timescale units. VFR is preserved: no averaging is applied.
func parseStts(data []byte, box *Box, sampleCount int64) ([]int64, error) {
	off := box.Payload + 4 // version+flags
	entryCount, err := readU32(data, off)
	if err != nil {
		return nil, err
	}
	off += 4
	durations := make([]int64, 0, sampleCount)
	for i := uint32(0); i < entryCount; i++ {
		count, err := readU32(data, off)
		if err != nil {
			return nil, err
		}
		delta, err := readU32(data, off+4)
		if err != nil {
			return nil, err
		}
		off += 8
		if delta == 0 {
			return nil, fmt.Errorf("%w: zero sample delta in stts entry %d",
				ErrBadTable, i)
		}
		for j := uint32(0); j < count; j++ {
			durations = append(durations, int64(delta))
		}
	}
	if int64(len(durations)) != sampleCount {
		return nil, fmt.Errorf("%w: stts expands to %d samples, stsz says %d",
			ErrBadTable, len(durations), sampleCount)
	}
	return durations, nil
}

// parseStss returns the set of keyframe sample numbers (1-based in the box).
func parseStss(data []byte, box *Box) (map[int]bool, error) {
	off := box.Payload + 4
	entryCount, err := readU32(data, off)
	if err != nil {
		return nil, err
	}
	off += 4
	keys := make(map[int]bool, entryCount)
	for i := uint32(0); i < entryCount; i++ {
		n, err := readU32(data, off)
		if err != nil {
			return nil, err
		}
		off += 4
		keys[int(n)] = true
	}
	return keys, nil
}

// parseStsz returns one size per sample.
func parseStsz(data []byte, box *Box) ([]int64, error) {
	off := box.Payload + 4 // version+flags
	// sample_size (uniform) and sample_count
	uniform, err := readU32(data, off)
	if err != nil {
		return nil, err
	}
	count, err := readU32(data, off+4)
	if err != nil {
		return nil, err
	}
	off += 8
	sizes := make([]int64, count)
	if uniform != 0 {
		for i := range sizes {
			sizes[i] = int64(uniform)
		}
		return sizes, nil
	}
	for i := uint32(0); i < count; i++ {
		sz, err := readU32(data, off)
		if err != nil {
			return nil, err
		}
		off += 4
		sizes[i] = int64(sz)
	}
	return sizes, nil
}

// parseChunkOffsets merges stco (32-bit) and co64 (64-bit) offsets.
func parseChunkOffsets(data []byte, roots []*Box) ([]int64, error) {
	var offsets []int64
	if box := First(roots, BoxCo64); box != nil {
		off := box.Payload + 4
		count, err := readU32(data, off)
		if err != nil {
			return nil, err
		}
		off += 4
		offsets = make([]int64, count)
		for i := uint32(0); i < count; i++ {
			v, err := readU64(data, off)
			if err != nil {
				return nil, err
			}
			offsets[i] = int64(v)
			off += 8
		}
		return offsets, nil
	}
	box := First(roots, BoxStco)
	if box == nil {
		return nil, fmt.Errorf("%w: no stco/co64", ErrMissingTable)
	}
	off := box.Payload + 4
	count, err := readU32(data, off)
	if err != nil {
		return nil, err
	}
	off += 4
	offsets = make([]int64, count)
	for i := uint32(0); i < count; i++ {
		v, err := readU32(data, off)
		if err != nil {
			return nil, err
		}
		offsets[i] = int64(v)
		off += 4
	}
	return offsets, nil
}

// BuildTracks resolves every trak into TrackInfo with exact per-sample ticks.
// Returned faults use track-local frame indices and byte offsets; the caller
// merges them onto the global timeline.
func BuildTracks(data []byte, roots []*Box, lim Limits) ([]TrackInfo, []fault.Fault, error) {
	traks := FindAll(roots, BoxTrak)
	if len(traks) > lim.MaxTracks {
		return nil, nil, fmt.Errorf("%w: %d tracks", ErrLimit, len(traks))
	}
	var infos []TrackInfo
	var faults []fault.Fault
	for trackIdx, trak := range traks {
		tid := uint32(trackIdx + 1)
		mdhd := findChild(trak, BoxMdhd)
		if mdhd == nil {
			return nil, nil, fmt.Errorf("%w: trak %d missing mdhd", ErrMissingTable, tid)
		}
		ts, err := readU32(data, mdhd.Payload+12) // timescale field offset
		if err != nil {
			return nil, nil, err
		}
		if _, err := clock.Rate(int64(ts)); err != nil {
			faults = append(faults, fault.New(fault.KindInvalidTimescale,
				fault.SeverityFatal, tid, -1, mdhd.Payload, 0,
				fmt.Sprintf("timescale %d invalid", ts)))
			return nil, faults, err
		}
		stts := findChild(trak, BoxStts)
		stsz := findChild(trak, BoxStsz)
		if stts == nil || stsz == nil {
			return nil, nil, fmt.Errorf("%w: trak %d missing stts/stsz", ErrMissingTable, tid)
		}
		sizes, err := parseStsz(data, stsz)
		if err != nil {
			return nil, nil, err
		}
		if int64(len(sizes)) > lim.MaxSamples {
			return nil, nil, fmt.Errorf("%w: trak %d sample count", ErrLimit, tid)
		}
		durations, err := parseStts(data, stts, int64(len(sizes)))
		if err != nil {
			return nil, nil, err
		}
		keySet := map[int]bool{}
		if stss := findChild(trak, BoxStss); stss != nil {
			keySet, err = parseStss(data, stss)
			if err != nil {
				return nil, nil, err
			}
		}
		chunks, err := parseChunkOffsets(data, trakChildren(trak))
		if err != nil {
			return nil, nil, err
		}
		rate, _ := clock.Rate(int64(ts))
		samples := make([]Sample, len(sizes))
		var cumulative int64
		for i := range sizes {
			dt, err := rate.Scale(durations[i])
			if err != nil {
				return nil, nil, fmt.Errorf("trak %d sample %d: %w", tid, i, err)
			}
			samples[i] = Sample{
				Index:         i,
				IsKeyframe:   len(keySet) == 0 || keySet[i+1],
				Size:          sizes[i],
				Offset:        chunkFor(i, chunks),
				DurationTicks: dt,
				StartTicks:    cumulative,
			}
			cumulative += dt
		}
		infos = append(infos, TrackInfo{
			TrackID: tid, Timescale: int64(ts),
			Samples: samples, Boxes: []*Box{trak, mdhd, stts, stsz},
		})
	}
	return infos, faults, nil
}

// chunkFor maps a sample index to its chunk offset. Without a parsed stsc the
// fixture builder uses one-sample chunks, so the indices line up directly.
func chunkFor(sampleIdx int, chunks []int64) int64 {
	if sampleIdx < len(chunks) {
		return chunks[sampleIdx]
	}
	if len(chunks) > 0 {
		return chunks[len(chunks)-1]
	}
	return -1
}

// findChild locates a box type inside a trak subtree.
func findChild(trak *Box, t BoxType) *Box {
	return First(trak.Child, t)
}

// trakChildren returns the subtree roots relevant to chunk offset lookup.
func trakChildren(trak *Box) []*Box {
	return trak.Child
}
