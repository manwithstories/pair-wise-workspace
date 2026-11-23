package pipeline

import (
	"sort"

	"mediasim/internal/clock"
	"mediasim/internal/fixture"
	"mediasim/internal/graph"
	"mediasim/internal/render"
	"mediasim/internal/rtp"
)

// scheduledFrame is one output frame mapped onto the media timebase.
type scheduledFrame struct {
	OutputIndex int   // 0..TotalFrames-1
	Ticks       int64 // timeline tick of the frame start
	CamSource   string
	CamFrame    int64
	Crossfade   bool
	Audio       render.AudioBlock
	DriftMs     float64
}

// schedule maps the timeline onto a fixed frame list. It is shared by the
// renderer and the sync calculation so both use the same timebase.
func schedule() []scheduledFrame {
	out := make([]scheduledFrame, 0, fixture.TotalFrames)
	for i := 0; i < fixture.TotalFrames; i++ {
		sf := scheduledFrame{OutputIndex: i, Ticks: int64(i) * fixture.VideoFrameTicks}
		switch {
		case i < fixture.Cam1StartFrame:
			sf.CamSource, sf.CamFrame = fixture.SourceCam0, int64(i)
		case i < fixture.Cam0Frames:
			// Crossfade: both cameras are live; the later one dominates.
			sf.CamSource, sf.CamFrame = fixture.SourceCam1, int64(i-fixture.Cam1StartFrame)
			sf.Crossfade = true
		default:
			sf.CamSource, sf.CamFrame = fixture.SourceCam1, int64(i-fixture.Cam1StartFrame)
		}
		out = append(out, sf)
	}
	return out
}

// audioBlockAt returns the audio samples covering a frame's tick range, cut
// on sample boundaries. The sample clock and the frame clock are both exact
// rationals, so a frame boundary generally falls between two samples: the
// block starts at the nearest sample and the residual is the reported sync
// error. Nothing is padded here; padding is the edit-point policy.
func audioBlockAt(frameStartTick int64) (render.AudioBlock, float64, error) {
	const samplesPerChunk = fixture.AudioDelta
	// Exact sample position as a rational: tick * 1024 / 1920.
	num := frameStartTick * samplesPerChunk
	start := num / fixture.AudioChunkTicks
	residual := num % fixture.AudioChunkTicks // 0..1919 of a 1920-tick chunk
	endNum := (frameStartTick + fixture.VideoFrameTicks) * samplesPerChunk
	end := endNum / fixture.AudioChunkTicks
	span := int(end - start)
	if span <= 0 {
		span = 1
	}
	src := make([]int16, span)
	for i := range src {
		src[i] = int16((start + int64(i)) % 4096)
	}
	b, err := render.CutAudio(src, start, start, int64(span))
	if err != nil {
		return render.AudioBlock{}, 0, err
	}
	// Residual in milliseconds: distance from the exact frame start to the
	// chosen sample. Always under one sample period.
	errMs := float64(residual) / float64(fixture.AudioChunkTicks) * 1000 / float64(fixture.AudioTimescale)
	return b, errMs, nil
}

// transportPlan is the deterministic packet schedule before loss.
func transportPlan() []rtp.Packet {
	packets := make([]rtp.Packet, 0, fixture.TotalFrames)
	for i := 0; i < fixture.TotalFrames; i++ {
		sf := scheduledFrame{CamFrame: int64(i), CamSource: fixture.SourceCam0}
		if i >= fixture.Cam1StartFrame {
			sf.CamFrame = int64(i - fixture.Cam1StartFrame)
			sf.CamSource = fixture.SourceCam1
		}
		p := rtp.Packet{
			Seq:      uint16(i),
			TS:       uint32(int64(i) * fixture.VideoFrameTicks),
			Frame:    i,
			Keyframe: sf.CamFrame%fixture.KeyframeEvery == 0,
		}
		packets = append(packets, p)
	}
	return packets
}

// buildGraphPlan validates the explicit timeline used by the demo and
// returns its deterministic processing order.
func buildGraphPlan() (*graph.Timeline, *graph.Plan, error) {
	base, err := clock.NewTimeline(fixture.TickRate, fixture.FPS)
	if err != nil {
		return nil, nil, err
	}
	cam0Ticks := int64(fixture.Cam0Frames) * fixture.VideoFrameTicks
	cam1Start := int64(fixture.Cam1StartFrame) * fixture.VideoFrameTicks
	cam1Dur := int64(fixture.Cam1Frames) * fixture.VideoFrameTicks
	audioDur := int64(fixture.AudioChunks) * fixture.AudioChunkTicks
	tl, err := graph.New(base, []graph.Track{
		{ID: "video", Kind: graph.TrackVideo, Segments: []graph.Segment{
			{ID: "cam0", Start: 0, Duration: cam0Ticks, Source: fixture.SourceCam0,
				EditPoints: []int64{cam0Ticks / 2}},
			{ID: "cam1", Start: cam1Start, Duration: cam1Dur, Source: fixture.SourceCam1},
		}},
		{ID: "voice", Kind: graph.TrackAudio, Segments: []graph.Segment{
			{ID: "voice0", Start: 0, Duration: audioDur, Source: fixture.SourceVoice,
				EditPoints: []int64{audioDur / 3, 2 * audioDur / 3}},
		}},
		{ID: "marks", Kind: graph.TrackMarker, Segments: []graph.Segment{
			{ID: "tac0", Start: 0, Duration: audioDur, Source: "tactical"},
		}},
	}, []graph.Transition{
		{ID: "xfade", From: "cam0", To: "cam1", Kind: graph.TransCrossfade,
			Duration: cam0Ticks - cam1Start},
	})
	if err != nil {
		return nil, nil, err
	}
	plan, err := graph.BuildPlan(tl)
	if err != nil {
		return nil, nil, err
	}
	return tl, plan, nil
}

// planSegments returns the plan's segment steps in processing order, which is
// the order the demuxer commits them.
func planSegments(p *graph.Plan) ([]graph.Step, error) {
	if p == nil {
		return nil, clock.Errorf(clock.CatGraph, clock.Span{}, clock.RecoveredNone, "missing plan")
	}
	out := make([]graph.Step, 0, len(p.Steps))
	for _, s := range p.Steps {
		if s.Kind == graph.NodeSegment {
			out = append(out, s)
		}
	}
	return out, nil
}

// sortedFramesOf returns frame numbers in ascending order.
func sortedFramesOf(m map[int]struct{}) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
