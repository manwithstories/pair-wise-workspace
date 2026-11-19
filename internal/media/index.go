// Package media inspects MP4 sample tables with go-mp4 and locates tail
// truncation and incomplete indexes to the byte, without aborting the batch
// on a single damaged segment.
package media

import (
	"os"

	mp4 "github.com/abema/go-mp4"

	"review/internal/issues"
)

// Report describes one inspected MP4 file.
type Report struct {
	Path           string
	Samples        int
	Delta          uint32 // stts sample delta (ticks per frame)
	DamageOffset   int64  // first suspicious byte offset, -1 when clean
	LastGoodSample int    // last fully parseable sample, -1 when unknown
	Kind           string // "", issues.MP4Truncated or issues.MP4IncompleteIdx
}

// Inspect walks the box tree, reads the sample table and cross-checks it
// against the file size. A damaged file yields a Report with a located
// damage offset, never an abort.
func Inspect(path string) (*Report, []issues.Issue) {
	rep := &Report{Path: path, DamageOffset: -1, LastGoodSample: -1}
	f, err := os.Open(path)
	if err != nil {
		return rep, []issues.Issue{{Kind: issues.MP4Unreadable, Path: path, Msg: err.Error()}}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return rep, []issues.Issue{{Kind: issues.MP4Unreadable, Path: path, Msg: err.Error()}}
	}
	fileSize := st.Size()

	var stts *mp4.Stts
	var stsc *mp4.Stsc
	var stsz *mp4.Stsz
	var stco *mp4.Stco
	var stcoEnd, lastBoxEnd uint64
	_, walkErr := mp4.ReadBoxStructure(f, func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(),
			mp4.BoxTypeMinf(), mp4.BoxTypeStbl():
			return h.Expand()
		case mp4.BoxTypeStts(), mp4.BoxTypeStsc(), mp4.BoxTypeStsz(), mp4.BoxTypeStco():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			switch b := box.(type) {
			case *mp4.Stts:
				stts = b
			case *mp4.Stsc:
				stsc = b
			case *mp4.Stsz:
				stsz = b
			case *mp4.Stco:
				stco = b
				stcoEnd = h.BoxInfo.Offset + h.BoxInfo.Size
			}
		}
		lastBoxEnd = h.BoxInfo.Offset + h.BoxInfo.Size
		return nil, nil
	})

	issue := func(kind string, off int64, msg string) []issues.Issue {
		rep.Kind = kind
		rep.DamageOffset = off
		return []issues.Issue{{Kind: kind, Path: path, ByteOffset: off, Msg: msg}}
	}

	if stsz == nil || stco == nil || stsc == nil {
		// Index unreadable (severe truncation or corrupt box tree).
		off := int64(lastBoxEnd)
		if walkErr != nil && off == 0 {
			off = 0
		}
		return rep, issue(issues.MP4Truncated, off, "sample table unreadable: "+errString(walkErr))
	}

	rep.Samples = int(stsz.SampleCount)
	if stts != nil && len(stts.Entries) > 0 {
		rep.Delta = stts.Entries[0].SampleDelta
	}
	sizes := stsz.EntrySize
	if stsz.SampleSize != 0 {
		sizes = make([]uint32, rep.Samples)
		for i := range sizes {
			sizes[i] = stsz.SampleSize
		}
	}
	if len(sizes) < rep.Samples {
		return rep, issue(issues.MP4IncompleteIdx, int64(lastBoxEnd),
			"stsz declares more samples than it lists")
	}

	// Expand the sample->chunk mapping from stsc.
	required := chunksNeeded(stsc.Entries, rep.Samples)
	perChunk := expandStsc(stsc.Entries, len(stco.ChunkOffset))
	if len(stco.ChunkOffset) < required {
		covered := 0
		for _, n := range perChunk {
			covered += int(n)
		}
		if covered > rep.Samples {
			covered = rep.Samples
		}
		rep.LastGoodSample = covered - 1
		return rep, issue(issues.MP4IncompleteIdx, int64(stcoEnd),
			"stco lists fewer chunk offsets than the sample table requires")
	}

	// Per-sample byte ranges; the first sample crossing EOF marks the
	// truncation point exactly.
	off := uint64(0)
	sample := 0
	for c := 0; c < len(perChunk) && sample < rep.Samples; c++ {
		off = uint64(stco.ChunkOffset[c])
		for k := uint32(0); k < perChunk[c] && sample < rep.Samples; k++ {
			end := off + uint64(sizes[sample])
			if end > uint64(fileSize) {
				rep.LastGoodSample = sample - 1
				return rep, issue(issues.MP4Truncated, fileSize,
					"sample data extends past end of file")
			}
			off = end
			sample++
		}
	}
	rep.LastGoodSample = rep.Samples - 1
	return rep, nil
}

func errString(err error) string {
	if err == nil {
		return "incomplete box tree"
	}
	return err.Error()
}

// expandStsc returns samples-per-chunk for each declared chunk.
func expandStsc(entries []mp4.StscEntry, chunks int) []uint32 {
	out := make([]uint32, 0, chunks)
	for i, e := range entries {
		next := chunks + 1
		if i+1 < len(entries) {
			next = int(entries[i+1].FirstChunk)
		}
		for c := int(e.FirstChunk); c < next && c <= chunks; c++ {
			out = append(out, e.SamplesPerChunk)
		}
	}
	return out
}

// chunksNeeded returns how many chunks the sample count requires.
func chunksNeeded(entries []mp4.StscEntry, samples int) int {
	n, remaining := 0, samples
	for i, e := range entries {
		span := 1
		if i+1 < len(entries) {
			span = int(entries[i+1].FirstChunk - e.FirstChunk)
		}
		for s := 0; s < span && remaining > 0; s++ {
			remaining -= int(e.SamplesPerChunk)
			n++
		}
	}
	if remaining > 0 && len(entries) > 0 {
		last := int64(entries[len(entries)-1].SamplesPerChunk)
		n += int((int64(remaining) + last - 1) / last)
	}
	return n
}
