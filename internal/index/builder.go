// Package index builds a per-sample random-access index (offset, size,
// pts, sync flag) for every track and persists it as JSON.
package index

import (
	"sort"
	"sync"

	"mp4probe/internal/box"
	"mp4probe/internal/moov"
	"mp4probe/internal/timeline"
)

// Version is the on-disk index format version.
const Version = 1

// TrackIndex is the expanded per-sample view of one track.
type TrackIndex struct {
	TrackID         uint32           `json:"track_id"`
	Handler         string           `json:"handler"`
	Codec           string           `json:"codec"`
	Timescale       uint32           `json:"timescale"`
	MediaDuration   uint64           `json:"media_duration"`
	Duration        uint64           `json:"duration"` // tkhd, movie timescale
	MovieTimescale  uint32           `json:"movie_timescale"`
	SampleCount     int              `json:"sample_count"`
	SyncSampleCount int              `json:"sync_sample_count"`
	ChunkCount      int              `json:"chunk_count"`
	ElstSegments    int              `json:"elst_segments"`
	ElstConflict    bool             `json:"elst_conflict"`
	WrapEvents      int              `json:"wrap_events"`
	AllSync         bool             `json:"all_sync"`
	Elst            []moov.ElstEntry `json:"elst,omitempty"`
	Offsets         []int64          `json:"offsets"`
	Sizes           []uint32         `json:"sizes"`
	PTS             []int64          `json:"pts"`
	DTS             []int64          `json:"dts"`
	Sync            []uint32         `json:"sync"` // 0-based sync sample indices
}

// Index is the full-file index.
type Index struct {
	Version      int           `json:"version"`
	File         string        `json:"file"`
	Size         uint64        `json:"size"`
	MoovOffset   uint64        `json:"moov_offset"`
	MdatOffset   uint64        `json:"mdat_offset"`
	MdatEnd      uint64        `json:"mdat_end"`
	UnknownBoxes int           `json:"unknown_boxes"`
	Tracks       []*TrackIndex `json:"tracks"`
}

// Track returns the index of the track with the given ID.
func (idx *Index) Track(id uint32) *TrackIndex {
	for _, t := range idx.Tracks {
		if t.TrackID == id {
			return t
		}
	}
	return nil
}

// Build expands every track in parallel and assembles the index,
// ordered by track ID. Sample offsets are validated against the mdat
// extent so a corrupt stco/co64 fails here, not during seek.
func Build(name string, fileSize uint64, m *moov.Moov) (*Index, error) {
	idx := &Index{
		Version:      Version,
		File:         name,
		Size:         fileSize,
		MoovOffset:   m.Offset,
		MdatOffset:   m.MdatOffset,
		MdatEnd:      m.MdatEnd,
		UnknownBoxes: m.UnknownBoxes,
		Tracks:       make([]*TrackIndex, len(m.Traks)),
	}
	var wg sync.WaitGroup
	errs := make([]error, len(m.Traks))
	for i, tr := range m.Traks {
		wg.Add(1)
		go func(i int, tr *moov.Trak) {
			defer wg.Done()
			ti, err := buildTrack(tr, m.MovieTimescale, m.MdatOffset, m.MdatEnd)
			if err != nil {
				errs[i] = err
				return
			}
			idx.Tracks[i] = ti
		}(i, tr)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(idx.Tracks, func(a, b int) bool {
		return idx.Tracks[a].TrackID < idx.Tracks[b].TrackID
	})
	return idx, nil
}

// buildTrack expands one track's tables into flat per-sample arrays.
func buildTrack(t *moov.Trak, movieScale uint32, mdatStart, mdatEnd uint64) (*TrackIndex, error) {
	n := t.SampleCount()
	ti := &TrackIndex{
		TrackID:        t.TrackID,
		Handler:        t.Handler,
		Codec:          t.Codec,
		Timescale:      t.Timescale,
		MediaDuration:  t.MediaDuration,
		Duration:       t.TkhdDuration,
		MovieTimescale: movieScale,
		SampleCount:    n,
		ChunkCount:     len(t.ChunkOffset),
		ElstSegments:   len(t.Elst),
		ElstConflict:   t.ElstConflict,
		Elst:           t.Elst,
	}
	offsets, sizes, err := expandSamples(t, mdatStart, mdatEnd)
	if err != nil {
		return nil, err
	}
	ti.Offsets, ti.Sizes = offsets, sizes
	dts, wraps, err := timeline.ExpandDTS(t.Stts, n)
	if err != nil {
		return nil, err
	}
	ti.DTS, ti.WrapEvents = dts, wraps
	pts, err := timeline.ComputePTS(dts, t.Ctts)
	if err != nil {
		return nil, err
	}
	ti.PTS = pts
	if t.HasStss {
		ti.Sync = make([]uint32, len(t.Stss))
		for i, s := range t.Stss {
			if s == 0 || int(s) > n {
				return nil, box.Errorf("build index", 0,
					"stss entry %d out of range [1,%d]", s, n)
			}
			ti.Sync[i] = s - 1 // store 0-based
		}
		ti.SyncSampleCount = len(ti.Sync)
	} else {
		ti.AllSync = true
		ti.SyncSampleCount = n
	}
	return ti, nil
}

// expandSamples walks stsc/stco(co64)/stsz(stz2) and yields each
// sample's absolute file offset and size. Chunk offsets must land
// inside the mdat extent.
func expandSamples(t *moov.Trak, mdatStart, mdatEnd uint64) ([]int64, []uint32, error) {
	nChunks := len(t.ChunkOffset)
	n := t.SampleCount()
	if nChunks == 0 && n > 0 {
		return nil, nil, box.Errorf("expand samples", 0, "track %d: no chunks for %d samples", t.TrackID, n)
	}
	if len(t.Stsc) == 0 && n > 0 {
		return nil, nil, box.Errorf("expand samples", 0, "track %d: empty stsc", t.TrackID)
	}
	for i, e := range t.Stsc {
		if e.FirstChunk == 0 || int(e.FirstChunk) > nChunks {
			return nil, nil, box.Errorf("expand samples", 0,
				"track %d: stsc entry %d first_chunk %d out of range [1,%d]", t.TrackID, i, e.FirstChunk, nChunks)
		}
		if i > 0 && e.FirstChunk <= t.Stsc[i-1].FirstChunk {
			return nil, nil, box.Errorf("expand samples", 0,
				"track %d: stsc first_chunk not ascending at entry %d", t.TrackID, i)
		}
		if e.SamplesPerChunk == 0 {
			return nil, nil, box.Errorf("expand samples", 0,
				"track %d: stsc entry %d has zero samples per chunk", t.TrackID, i)
		}
	}
	offsets := make([]int64, 0, n)
	sizes := make([]uint32, 0, n)
	sample := 0
	entry := 0
	for chunk := 1; chunk <= nChunks; chunk++ {
		for entry+1 < len(t.Stsc) && int(t.Stsc[entry+1].FirstChunk) <= chunk {
			entry++
		}
		spc := int(t.Stsc[entry].SamplesPerChunk)
		off := t.ChunkOffset[chunk-1]
		if off < mdatStart || off >= mdatEnd {
			return nil, nil, box.Errorf("expand samples", off,
				"track %d: chunk %d offset outside mdat [%d,%d)", t.TrackID, chunk, mdatStart, mdatEnd)
		}
		for s := 0; s < spc; s++ {
			if sample >= n {
				if chunk == nChunks {
					break // last chunk may legitimately be short
				}
				return nil, nil, box.Errorf("expand samples", off,
					"track %d: stsc/stsz mismatch: chunk %d yields more than %d samples", t.TrackID, chunk, n)
			}
			size := t.Sizes[sample]
			if off+uint64(size) > mdatEnd {
				return nil, nil, box.Errorf("expand samples", off,
					"track %d: sample %d end %d exceeds mdat end %d", t.TrackID, sample, off+uint64(size), mdatEnd)
			}
			offsets = append(offsets, int64(off))
			sizes = append(sizes, size)
			off += uint64(size)
			sample++
		}
	}
	if sample != n {
		return nil, nil, box.Errorf("expand samples", 0,
			"track %d: stsc/stsz mismatch: expanded %d of %d samples", t.TrackID, sample, n)
	}
	return offsets, sizes, nil
}
