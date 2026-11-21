package probe

import (
	"bytes"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"mediacheck/internal/clock"
	"mediacheck/internal/model"
)

// Result is the full probe output for one asset.
type Result struct {
	InputID   string
	Tracks    []*Track
	Integrity *IntegrityReport
	// MediaErrors are mapped onto the shared model vocabulary and carry
	// tick ranges on the unified timeline (byte-only defects use range [0,0)
	// with the offset embedded in Detail).
	MediaErrors []*model.MediaError
	FileSize    int64

	head    []byte     // bounded moov-region buffer, reused by later passes
	rawList []*rawIndex
}

// Parse reads an MP4 from r (size = total bytes) and produces unified
// timeline tracks plus an integrity report. Reads are bounded to declared
// boxes; a truncated tail is reported, never silently accepted.
func Parse(r io.ReaderAt, size int64, inputID string) (*Result, error) {
	res := &Result{InputID: inputID, FileSize: size, Integrity: &IntegrityReport{FileSize: size}}

	span, truncAt, scanErr := scanTopLevel(r, size)
	res.Integrity.Boxes = span.boxes
	res.Integrity.Mdat = span.mdat
	if scanErr != nil {
		return nil, scanErr // unreadable header, nothing trustworthy yet
	}
	if truncAt >= 0 {
		res.Integrity.Defects = append(res.Integrity.Defects, probeDefect{
			code: string(model.CodeContainerTruncated),
			start: truncAt, end: size,
			detail: fmt.Sprintf("declared boxes run past EOF at byte %d", truncAt),
		})
	}

	res.head = extractHead(r, size)
	seeker := bytes.NewReader(res.head)
	_, err := mp4.ReadBoxStructure(seeker, func(h *mp4.ReadHandle) (interface{}, error) {
		// Only container expansion is needed here; table extraction happens
		// over the same bounded head buffer via collectIndexes.
		switch h.BoxInfo.Type.String() {
		case "moov", "trak", "mdia", "minf", "stbl", "edts":
			return h.Expand()
		}
		return nil, nil
	})
	if err != nil {
		return nil, fmt.Errorf("probe: structure parse failed: %w", err)
	}

	res.rawList = collectIndexes(res.head)
	// Rebuild tracks from the raw tables captured during the walk.
	for _, idx := range res.rawList {
		tr, terr := buildTrack(idx)
		if terr != nil {
			res.MediaErrors = append(res.MediaErrors, terr)
			continue
		}
		res.Tracks = append(res.Tracks, tr)
	}

	res.evaluateIntegrity()
	res.promoteDefects()
	return res, nil
}

// extractHead returns the file prefix up to mdat payload start (or EOF),
// which is the only region go-mp4 needs; sample payloads stay bounded.
func extractHead(r io.ReaderAt, size int64) []byte {
	limit := size
	if limit > maxHeadBytes {
		limit = maxHeadBytes
	}
	buf := make([]byte, limit)
	n, _ := r.ReadAt(buf, 0)
	return buf[:n]
}

// buildTrack converts raw stbl tables into unified-timeline samples.
func buildTrack(idx *rawIndex) (*Track, *model.MediaError) {
	if idx.timescale == 0 {
		return nil, model.NewError(model.CodeIndexMissingPage, "track", 0, 0,
			"missing or zero timescale in mdhd")
	}
	tr := &Track{
		ID: idx.trackID, IsVideo: idx.isVideo, Timescale: idx.timescale,
		Edits: idx.edits, StartDTS: idx.firstDTS(), Duration: idx.duration,
	}
	dts := idx.firstDTS()
	for i := uint32(0); i < uint32(len(idx.sizes)); i++ {
		delta := idx.deltaAt(i)
		_, key := idx.keyframes[i+1] // stss numbers are 1-based
		s := Sample{
			Index: i, IsKeyframe: key || (!idx.isVideo), Size: idx.sizes[i],
			DTS: dts, PTS: dts,
		}
		if i < uint32(len(idx.chunks)) {
			s.ChunkOffset = idx.chunks[i]
		}
		s.StartTicks = clock.ToTicks(s.PTS, idx.timescale)
		s.EndTicks = clock.ToTicks(s.PTS+int64(delta), idx.timescale)
		tr.Samples = append(tr.Samples, s)
		dts += int64(delta)
	}
	applyEdits(tr, idx)
	if len(tr.Samples) > 0 {
		tr.TimelineStart = tr.Samples[0].StartTicks
	}
	return tr, nil
}

// applyEdits shifts presentation times according to the (possibly empty)
// edit list using integer arithmetic only.
func applyEdits(tr *Track, idx *rawIndex) {
	if len(idx.edits) == 0 {
		return
	}
	var mediaOrigin int64
	for _, e := range idx.edits {
		if e.MediaTime >= 0 {
			mediaOrigin = e.MediaTime
			break
		}
	}
	for i := range tr.Samples {
		tr.Samples[i].PTS = tr.Samples[i].DTS - mediaOrigin
		tr.Samples[i].StartTicks = clock.ToTicks(tr.Samples[i].PTS, idx.timescale)
		tr.Samples[i].EndTicks = clock.ToTicks(
			tr.Samples[i].PTS+tr.sampleDur(i), idx.timescale)
	}
	// The first non-empty edit starts the presentation timeline at zero.
	tr.TimelineStart = 0
}

func (t *Track) sampleDur(i int) int64 {
	if i+1 < len(t.Samples) {
		return t.Samples[i+1].DTS - t.Samples[i].DTS
	}
	if len(t.Samples) >= 2 {
		return t.Samples[1].DTS - t.Samples[0].DTS
	}
	return 0
}

func (idx *rawIndex) firstDTS() int64 {
	if len(idx.edits) > 0 {
		for _, e := range idx.edits {
			if e.MediaTime > 0 {
				return e.MediaTime
			}
		}
	}
	return 0
}

func (idx *rawIndex) deltaAt(i uint32) uint32 {
	var consumed uint32
	for _, e := range idx.stts {
		if i < consumed+e.SampleCount {
			return e.SampleDelta
		}
		consumed += e.SampleCount
	}
	if len(idx.stts) > 0 {
		return idx.stts[len(idx.stts)-1].SampleDelta
	}
	return 0
}
