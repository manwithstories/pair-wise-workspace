// Package mp4scan extracts sample tables (stts/stsz/stco/stsc/stss/ctts)
// from local MP4 files, validates sample boundaries against the actual
// file size, and registers damaged intervals in the shared error model.
// It never fabricates durations for missing data.
package mp4scan

import (
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"

	"mediaqa/internal/media"
)

// Sample is one entry of a track's sample table with absolute file offset.
type Sample struct {
	Index    int
	DTS      int64 // decode timestamp, track timescale ticks
	PTS      int64 // presentation timestamp (DTS + ctts offset)
	Duration int64 // sample duration in ticks
	Size     uint32
	Offset   uint64 // absolute file offset of the sample payload
	Key      bool   // sync sample (random access point)
	Corrupt  bool   // payload extends beyond the file or a truncated mdat
}

// Track is a scanned track sample table.
type Track struct {
	ID        uint32
	Kind      string // "video" or "audio" (from hdlr)
	Timescale int64
	Samples   []Sample
}

// Result is the outcome of scanning one file.
type Result struct {
	Tracks    []Track
	MoovFound bool
}

// rawTrack accumulates box payloads for one trak.
type rawTrack struct {
	id        uint32
	kind      string
	timescale int64
	stts      []mp4.SttsEntry
	ctts      *mp4.Ctts
	sizes     []uint32
	chunks    []uint64
	stsc      []mp4.StscEntry
	sync      map[uint32]bool // 1-based; nil means all sync
	hasStss   bool
}

// Scan reads the MP4 at path and builds sample tables. Structural
// problems are appended to errs (track key is the file track ID); only
// I/O failures are returned as Go errors.
func Scan(path string, errs *media.List) (*Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	fileSize := st.Size()

	res := &Result{}
	var tracks []*rawTrack
	cur := func() *rawTrack {
		if len(tracks) == 0 {
			tracks = append(tracks, &rawTrack{})
		}
		return tracks[len(tracks)-1]
	}

	_, err = mp4.ReadBoxStructure(f, func(h *mp4.ReadHandle) (interface{}, error) {
		depth := len(h.Path)
		switch h.BoxInfo.Type {
		case mp4.BoxTypeMoov():
			res.MoovFound = true
			return h.Expand()
		case mp4.BoxTypeTrak():
			tracks = append(tracks, &rawTrack{})
			return h.Expand()
		case mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl():
			return h.Expand()
		case mp4.BoxTypeMdat():
			if depth == 1 && int64(h.BoxInfo.Offset)+int64(h.BoxInfo.Size) > fileSize {
				errs.Addf(media.SeverityRecoverable, "", "truncated-mdat",
					media.Rational{}, media.Rational{},
					"mdat at offset %d declares %d bytes but file is %d bytes",
					h.BoxInfo.Offset, h.BoxInfo.Size, fileSize)
			}
			return nil, nil
		}
		if depth < 2 { // only payloads inside a trak are relevant
			return nil, nil
		}
		box, _, err := h.ReadPayload()
		if err != nil {
			return nil, err
		}
		t := cur()
		switch b := box.(type) {
		case *mp4.Tkhd:
			t.id = b.TrackID
		case *mp4.Mdhd:
			t.timescale = int64(b.Timescale)
		case *mp4.Hdlr:
			switch string(b.HandlerType[:]) {
			case "vide":
				t.kind = "video"
			case "soun":
				t.kind = "audio"
			}
		case *mp4.Stts:
			t.stts = b.Entries
		case *mp4.Ctts:
			t.ctts = b
		case *mp4.Stsz:
			t.sizes = b.EntrySize
			if b.SampleSize != 0 && len(t.sizes) == 0 {
				t.sizes = make([]uint32, b.SampleCount)
				for i := range t.sizes {
					t.sizes[i] = b.SampleSize
				}
			}
		case *mp4.Stco:
			for _, o := range b.ChunkOffset {
				t.chunks = append(t.chunks, uint64(o))
			}
		case *mp4.Co64:
			t.chunks = append(t.chunks, b.ChunkOffset...)
		case *mp4.Stsc:
			t.stsc = b.Entries
		case *mp4.Stss:
			t.hasStss = true
			t.sync = map[uint32]bool{}
			for _, n := range b.SampleNumber {
				t.sync[n] = true
			}
		}
		return nil, nil
	})
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if !res.MoovFound {
		errs.Addf(media.SeverityRecoverable, "", "missing-moov",
			media.Rational{}, media.Rational{},
			"no moov box; sample index unavailable, boundaries not fabricated")
	}
	for _, rt := range tracks {
		res.Tracks = append(res.Tracks, buildTrack(rt, fileSize, errs))
	}
	return res, nil
}

// buildTrack expands run-length tables into a per-sample list and flags
// samples whose payload is not fully present in the file.
func buildTrack(rt *rawTrack, fileSize int64, errs *media.List) Track {
	tr := Track{ID: rt.id, Kind: rt.kind, Timescale: rt.timescale}
	n := len(rt.sizes)
	if n == 0 {
		return tr
	}
	// Expand stts into per-sample DTS/duration.
	dts := make([]int64, n)
	dur := make([]int64, n)
	var t int64
	idx := 0
	for _, e := range rt.stts {
		for i := uint32(0); i < e.SampleCount && idx < n; i++ {
			dts[idx] = t
			dur[idx] = int64(e.SampleDelta)
			t += int64(e.SampleDelta)
			idx++
		}
	}
	// Map samples to chunk offsets via stsc.
	offsets := make([]uint64, n)
	sample := 0
	entry := 0
	for chunk := 1; chunk <= len(rt.chunks) && sample < n; chunk++ {
		for entry+1 < len(rt.stsc) && uint32(chunk) >= rt.stsc[entry+1].FirstChunk {
			entry++
		}
		perChunk := int(rt.stsc[entry].SamplesPerChunk)
		off := rt.chunks[chunk-1]
		for k := 0; k < perChunk && sample < n; k++ {
			offsets[sample] = off
			off += uint64(rt.sizes[sample])
			sample++
		}
	}
	// Assemble samples; flag corruption.
	var corruptRunStart = -1
	flush := func(end int) {
		if corruptRunStart >= 0 && end > corruptRunStart {
			s0, s1 := tr.Samples[corruptRunStart], tr.Samples[end-1]
			ts := rt.timescale
			if ts <= 0 {
				ts = 1
			}
			errs.Addf(media.SeverityRecoverable, fmt.Sprintf("file-track-%d", rt.id),
				"corrupt-samples",
				media.FromTicks(s0.PTS, ts), media.FromTicks(s1.PTS+s1.Duration, ts),
				"%d sample(s) beyond file boundary (index %d..%d)", end-corruptRunStart, s0.Index, s1.Index)
		}
		corruptRunStart = -1
	}
	for i := 0; i < n; i++ {
		s := Sample{
			Index:    i,
			DTS:      dts[i],
			PTS:      dts[i],
			Duration: dur[i],
			Size:     rt.sizes[i],
			Offset:   offsets[i],
			Key:     !rt.hasStss || rt.sync[uint32(i+1)],
		}
		if rt.ctts != nil {
			s.PTS = dts[i] + rt.ctts.GetSampleOffset(i)
		}
		if int64(s.Offset)+int64(s.Size) > fileSize {
			s.Corrupt = true
			if corruptRunStart < 0 {
				corruptRunStart = i
			}
		} else {
			flush(i)
		}
		tr.Samples = append(tr.Samples, s)
	}
	flush(n)
	return tr
}
