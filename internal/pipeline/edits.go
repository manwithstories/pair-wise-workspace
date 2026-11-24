package pipeline

import (
	"mediasim/internal/clock"
	"mediasim/internal/fixture"
	"mediasim/internal/graph"
	"mediasim/internal/render"
)

// AudioEdit reports one cut where an edit point split an audio block.
type AudioEdit struct {
	Segment        string  `json:"segment"`
	CutTick        int64   `json:"cut_tick"`
	CutSample      int64   `json:"cut_sample"`
	PaddingSamples int     `json:"padding_samples"`
	Policy         string  `json:"policy"`
	SyncErrorMs    float64 `json:"sync_error_ms"`
}

// EditPolicyPadSilence is the single, fixed strategy for a cut that lands
// between samples: drop the one sample straddling the cut, replace it with a
// sample of silence, and start the right-hand block on the next real sample.
// The discarded sub-sample fraction is reported as sync error and is never
// hidden by time-shifting the block.
const EditPolicyPadSilence = "pad_leading_silence"

// applyEditPoints cuts every audio segment at its declared edit points and
// returns the resulting right-hand blocks with a diagnostic per cut. Cuts are
// visited in plan order, so identical timelines always produce identical cuts.
func applyEditPoints(tl *graph.Timeline, plan *graph.Plan) ([]render.AudioBlock, []AudioEdit, error) {
	if tl == nil || plan == nil {
		return nil, nil, clock.Errorf(clock.CatGraph, clock.Span{}, clock.RecoveredNone,
			"missing timeline or plan")
	}
	audioSegs := map[string]graph.Segment{}
	for _, tr := range tl.Tracks {
		if tr.Kind != graph.TrackAudio {
			continue
		}
		for _, s := range tr.Segments {
			audioSegs[s.ID] = s
		}
	}

	var blocks []render.AudioBlock
	var edits []AudioEdit
	for _, step := range plan.Steps {
		if step.Kind != graph.NodeSegment {
			continue
		}
		seg, ok := audioSegs[step.Segment]
		if !ok {
			continue
		}
		for _, editOffset := range seg.EditPoints {
			block, edit, err := cutAt(seg, editOffset)
			if err != nil {
				return nil, nil, err
			}
			blocks = append(blocks, block)
			edits = append(edits, edit)
		}
	}
	return blocks, edits, nil
}

// cutAt produces the right-hand block for one edit point.
func cutAt(seg graph.Segment, editOffset int64) (render.AudioBlock, AudioEdit, error) {
	cutTick := seg.Start + editOffset
	const (
		samplesPerChunk = fixture.AudioDelta
		ticksPerChunk   = fixture.AudioChunkTicks
	)
	// Exact sample position of the cut on the 48 kHz sample clock.
	num := cutTick * samplesPerChunk
	q, r := num/ticksPerChunk, num%ticksPerChunk

	// A sample cannot be split, so a cut between samples discards the
	// straddling sample and starts the block on the next whole sample.
	firstReal := q
	padding := 0
	if r != 0 {
		firstReal = q + 1
		padding = 1
	}
	span := int((seg.Duration - editOffset) * samplesPerChunk / ticksPerChunk)
	if span <= 0 {
		span = 1
	}
	if span > samplesPerChunk {
		span = samplesPerChunk
	}
	src := make([]int16, span)
	for i := range src {
		src[i] = int16((firstReal + int64(i)) % 4096)
	}
	block, err := render.CutAudio(src, firstReal, firstReal, int64(span))
	if err != nil {
		return render.AudioBlock{}, AudioEdit{}, clock.WithSpan(err,
			clock.NewSpan("voice", seg.ID, clock.Interval{Start: cutTick,
				End: cutTick + fixture.VideoFrameTicks}))
	}
	block = render.PadSilence(block, padding)
	block.Start = q // block position on the sample clock, before padding

	// Time from the true cut to the first real sample. r/ticksPerChunk is the
	// fractional part of a sample, so the gap is that complement in samples.
	var syncMs float64
	if r != 0 {
		missingSamples := float64(ticksPerChunk-r) / float64(ticksPerChunk)
		syncMs = missingSamples / float64(fixture.AudioTimescale) * 1000
	}
	return block, AudioEdit{
		Segment:        seg.ID,
		CutTick:        cutTick,
		CutSample:      firstReal,
		PaddingSamples: padding,
		Policy:         EditPolicyPadSilence,
		SyncErrorMs:    syncMs,
	}, nil
}
