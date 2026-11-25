// Package mp4 scans H.264 proxy MP4 sample tables, reconstructs per-sample
// timestamps from stts (VFR), and measures keyframe gaps from stss. It reads
// metadata only and never decodes media.
package mp4

import (
	"fmt"
	"io"
	"os"

	gomp4 "github.com/abema/go-mp4"

	"mediaaudit/internal/model"
)

// Sample is one decoded video sample with its timeline position.
type Sample struct {
	Index        int     // 0-based
	DTSNanos     int64   // decode timestamp in ns
	DeltaTicks   uint32  // stts delta in movie timescale
	IsKeyframe   bool
}

// TrackReport describes a video track's sample table.
type TrackReport struct {
	Timescale      uint32
	SampleCount    int
	Samples        []Sample
	KeyframeGapsMs []float64 // gap between consecutive keyframes, in ms
	MaxGapMs       float64
	MinGapMs       float64
	IsVFR          bool
}

// ScanResult holds every video track found in the file.
type ScanResult struct {
	Tracks []TrackReport
	Faults *model.FaultSet
}

// boxCapture collects the boxes we care about during the structural walk.
type boxCapture struct {
	mdhd     *gomp4.Mdhd
	stts     *gomp4.Stts
	stss     *gomp4.Stss
	hasStss  bool
}

// Scan opens path and extracts H.264 video sample timing and keyframe gaps.
func Scan(path string) (*ScanResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return scanSeeker(f)
}

func scanSeeker(r io.ReadSeeker) (*ScanResult, error) {
	res := &ScanResult{Faults: model.NewFaultSet()}
	// Per-track capture is selected by path depth; we keep the most recent
	// mdhd/stts/stss group seen inside a moov/trak/mdia subtree.
	var current *boxCapture
	groups := make([]*boxCapture, 0)

	handler := func(h *gomp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case gomp4.BoxTypeMdhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			current.mdhd = box.(*gomp4.Mdhd)
		case gomp4.BoxTypeStts():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			current.stts = box.(*gomp4.Stts)
		case gomp4.BoxTypeStss():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			current.stss = box.(*gomp4.Stss)
			current.hasStss = true
		case gomp4.BoxTypeTrak():
			// Starting a new trak: reset per-track capture. A pointer is kept
			// so boxes visited as descendants mutate the same captured group.
			current = &boxCapture{}
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
			groups = append(groups, current)
			return nil, nil
		}
		return h.Expand()
	}

	if _, err := gomp4.ReadBoxStructure(r, handler); err != nil {
		return nil, model.NewFault(model.KindInvalid, "", model.FrameRange{}, 0,
			"mp4 structure: "+err.Error())
	}

	for _, g := range groups {
		if g.mdhd == nil || g.stts == nil {
			continue
		}
		tr, err := buildTrack(g)
		if err != nil {
			res.Faults.AddErr(err)
			continue
		}
		res.Tracks = append(res.Tracks, tr)
	}
	if len(res.Tracks) == 0 {
		return nil, model.NewFault(model.KindInvalid, "", model.FrameRange{}, 0,
			"no video sample table (stts/mdhd) found")
	}
	return res, nil
}

func buildTrack(g *boxCapture) (TrackReport, error) {
	scale := g.mdhd.Timescale
	if scale == 0 {
		return TrackReport{}, model.NewFault(model.KindInvalid, "",
			model.FrameRange{}, -1, "mdhd timescale is zero")
	}
	rate := model.Hz(int64(scale))
	tr := TrackReport{Timescale: scale, MinGapMs: -1}

	// Expand stts into per-sample deltas and cumulative DTS.
	keySet := map[uint32]bool{}
	if g.hasStss && g.stss != nil {
		for _, sn := range g.stss.SampleNumber {
			keySet[sn] = true // stss sample numbers are 1-based
		}
	}
	var dtsTicks uint64
	idx := 0
	firstDelta := uint32(0)
	for e, entry := range g.stts.Entries {
		if entry.SampleDelta == 0 {
			return tr, model.NewFault(model.KindInvalid, "",
				model.FrameRange{First: model.Frame(idx)}, -1,
				"stts contains zero delta")
		}
		if e == 0 {
			firstDelta = entry.SampleDelta
		}
		if entry.SampleDelta != firstDelta {
			tr.IsVFR = true
		}
		for k := uint32(0); k < entry.SampleCount; k++ {
			s := Sample{
				Index:      idx,
				DTSNanos:   rate.Nanos(int64(dtsTicks)),
				DeltaTicks: entry.SampleDelta,
				IsKeyframe: !g.hasStss || keySet[uint32(idx+1)],
			}
			tr.Samples = append(tr.Samples, s)
			dtsTicks += uint64(entry.SampleDelta)
			idx++
		}
	}
	tr.SampleCount = len(tr.Samples)
	computeKeyframeGaps(&tr)
	return tr, nil
}

func computeKeyframeGaps(tr *TrackReport) {
	var prevNS int64
	havePrev := false
	for _, s := range tr.Samples {
		if !s.IsKeyframe {
			continue
		}
		if havePrev {
			gapMs := float64(s.DTSNanos-prevNS) / 1e6
			tr.KeyframeGapsMs = append(tr.KeyframeGapsMs, gapMs)
			if gapMs > tr.MaxGapMs {
				tr.MaxGapMs = gapMs
			}
			if tr.MinGapMs < 0 || gapMs < tr.MinGapMs {
				tr.MinGapMs = gapMs
			}
		}
		prevNS = s.DTSNanos
		havePrev = true
	}
	if tr.MinGapMs < 0 {
		tr.MinGapMs = 0
	}
}

// String gives a compact one-line summary for terminal output.
func (r *TrackReport) String() string {
	return fmt.Sprintf("track scale=%d samples=%d vfr=%t keyframe_gaps=%d max=%.2fms",
		r.Timescale, r.SampleCount, r.IsVFR, len(r.KeyframeGapsMs), r.MaxGapMs)
}
