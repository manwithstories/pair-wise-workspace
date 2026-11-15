// Package export writes presentation-range sample manifests (offset,
// size, SHA-256 per sample) and re-verifies them against the media
// file. It reuses the shared box/index layers; mdat is read strictly
// by offset.
package export

import (
	"io"
	"sort"

	"mp4probe/internal/box"
	"mp4probe/internal/index"
	"mp4probe/internal/seek"
	"mp4probe/internal/timeline"
)

// SampleEntry is one manifest line.
type SampleEntry struct {
	SampleIndex int    `json:"sample_index"`
	PTS         int64  `json:"pts"` // presentation, track timescale
	MediaPTS    int64  `json:"media_pts"`
	DTS         int64  `json:"dts"`
	FrameType   string `json:"frame_type"`
	Offset      int64  `json:"offset"`
	Size        uint32 `json:"size"`
	SHA256      string `json:"sha256"`
}

// Manifest is the exported range listing.
type Manifest struct {
	File    string        `json:"file"`
	TrackID uint32        `json:"track_id"`
	From    string        `json:"from"`
	To      string        `json:"to"`
	FromPts int64         `json:"from_pts"` // presentation, track timescale
	ToPts   int64         `json:"to_pts"`
	Count   int           `json:"count"`
	Samples []SampleEntry `json:"samples"`
}

// Range exports every sample of trackID whose presentation timestamp
// falls into [fromPres, toPres) (movie timescale). Samples are listed
// in presentation order so ranges crossing edit list segments or a
// 2^32 wrap stay continuous. Any sample extending past mdat fails the
// whole export.
func Range(idx *index.Index, trackID uint32, mapper *timeline.Mapper, r io.ReaderAt,
	fromPres, toPres int64, fromStr, toStr string) (*Manifest, error) {
	tr := idx.Track(trackID)
	if tr == nil {
		return nil, seek.ErrLocate
	}
	if fromPres < 0 || toPres <= fromPres {
		return nil, box.Errorf("export range", 0, "invalid range [%d,%d)", fromPres, toPres)
	}
	loc, err := seek.NewLocator(idx, trackID, mapper, r)
	if err != nil {
		return nil, err
	}
	m := &Manifest{
		File:    idx.File,
		TrackID: trackID,
		From:    fromStr,
		To:      toStr,
		FromPts: fromPres * int64(mapper.TrackScale) / int64(mapper.MovieScale),
		ToPts:   toPres * int64(mapper.TrackScale) / int64(mapper.MovieScale),
	}
	for i := 0; i < tr.SampleCount; i++ {
		pres, ok := mapper.ToPresentation(tr.PTS[i])
		if !ok || pres < fromPres || pres >= toPres {
			continue
		}
		off := uint64(tr.Offsets[i])
		if off+uint64(tr.Sizes[i]) > idx.MdatEnd {
			return nil, box.Errorf("export range", off,
				"sample %d end %d exceeds mdat end %d", i, off+uint64(tr.Sizes[i]), idx.MdatEnd)
		}
		h, err := seek.HashSample(r, tr.Offsets[i], tr.Sizes[i])
		if err != nil {
			return nil, err
		}
		frameType := "non-sync"
		if loc.IsSync(i) {
			frameType = "sync"
		}
		m.Samples = append(m.Samples, SampleEntry{
			SampleIndex: i,
			PTS:         pres * int64(mapper.TrackScale) / int64(mapper.MovieScale),
			MediaPTS:    tr.PTS[i],
			DTS:         tr.DTS[i],
			FrameType:   frameType,
			Offset:      tr.Offsets[i],
			Size:        tr.Sizes[i],
			SHA256:      h,
		})
	}
	sort.SliceStable(m.Samples, func(a, b int) bool {
		return m.Samples[a].PTS < m.Samples[b].PTS
	})
	m.Count = len(m.Samples)
	return m, nil
}

// Mismatch reports one sample whose recomputed hash differs.
type Mismatch struct {
	SampleIndex int    `json:"sample_index"`
	Expected    string `json:"expected"`
	Actual      string `json:"actual"`
}

// VerifyResult summarises a verify run.
type VerifyResult struct {
	CheckedSamples  int        `json:"checked_samples"`
	Mismatches      int        `json:"mismatches"`
	MismatchSamples []Mismatch `json:"mismatch_samples,omitempty"`
}

// Verify recomputes the SHA-256 of every manifest sample from r and
// compares it against the recorded value.
func Verify(r io.ReaderAt, m *Manifest) (*VerifyResult, error) {
	res := &VerifyResult{}
	for _, s := range m.Samples {
		h, err := seek.HashSample(r, s.Offset, s.Size)
		if err != nil {
			return nil, err
		}
		res.CheckedSamples++
		if h != s.SHA256 {
			res.Mismatches++
			res.MismatchSamples = append(res.MismatchSamples, Mismatch{
				SampleIndex: s.SampleIndex,
				Expected:    s.SHA256,
				Actual:      h,
			})
		}
	}
	return res, nil
}
