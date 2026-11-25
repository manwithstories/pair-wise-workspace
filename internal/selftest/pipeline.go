package selftest

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"medpipe/internal/audio"
	"medpipe/internal/clock"
	"medpipe/internal/fingerprint"
	"medpipe/internal/mp4"
	"medpipe/internal/pipe"
	"medpipe/internal/resume"
	"medpipe/internal/segment"
)

// segmentTargetMillis is the fixed cut cadence for the self-test fixtures.
const segmentTargetMillis = 1000

// inflightBudget is the acceptance-required 8 MiB in-flight ceiling.
const inflightBudget int64 = 8 << 20

// PipelineResult aggregates everything the self-test asserts on.
type PipelineResult struct {
	InputPath     string
	InputSha256   string
	Plan          *segment.Plan
	SilenceMillis int
	DedupHits     int
	Segments      []resume.SegmentState
	PeakInflight  int64
	OutDir        string
	Store         *resume.Store
}

// Process runs the full pipeline over one good input. When startFrom > 0 the
// planner still emits every boundary but the runner skips already-committed
// segments (resume path).
func Process(ctx context.Context, inputPath, workDir string,
	cp *resume.Checkpoint, startFrom int) (*PipelineResult, error) {
	// Segments are emitted directly under <dir>/out so the acceptance
	// command's shasum target "$D/out/seg-000.mp4" resolves without nesting.
	outDir := filepath.Join(workDir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, clock.E(clock.KindOther, "selftest.pipeline", clock.WithWrap(err))
	}

	inputSum, err := resume.HashFile(inputPath)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(inputPath)
	if err != nil {
		return nil, clock.E(clock.KindOther, "selftest.pipeline", clock.WithWrap(err))
	}
	container, err := mp4.Parse(f, mp4.DefaultLimits)
	f.Close()
	if err != nil {
		return nil, err
	}

	planner, err := segment.NewPlanner(container)
	if err != nil {
		return nil, err
	}
	plan, err := planner.Build(segmentTargetMillis)
	if err != nil {
		return nil, err
	}

	silence, err := detectSilence(ctx, inputPath, container)
	if err != nil {
		return nil, err
	}

	store, err := resume.NewStore(workDir)
	if err != nil {
		return nil, err
	}
	writer, err := segment.NewWriter(outDir)
	if err != nil {
		return nil, err
	}
	deduper := fingerprint.NewDeduper()
	geo := fingerprint.DefaultGeometry

	res := &PipelineResult{
		InputPath: inputPath, InputSha256: inputSum, Plan: plan,
		SilenceMillis: silence, OutDir: outDir, Store: store,
	}

	metrics, err := res.runStages(ctx, container, plan, cp, startFrom,
		store, writer, deduper, geo, inputSum)
	if err != nil {
		return nil, err
	}
	res.PeakInflight = metrics.PeakInflight
	res.DedupHits = deduper.Hits()
	res.Segments = cp.Segments
	return res, nil
}

// runStages drives analyze -> emit through the bounded pipe. Analyze builds a
// normalized fingerprint window; emit dedups, writes the segment and updates
// the checkpoint. The shared byte budget bounds memory across both stages.
func (r *PipelineResult) runStages(ctx context.Context, container *mp4.Container,
	plan *segment.Plan, cp *resume.Checkpoint, startFrom int,
	store *resume.Store, writer *segment.Writer,
	deduper *fingerprint.Deduper, geo fingerprint.FrameGeometry,
	inputSum string) (pipe.Metrics, error) {

	stages := []pipe.Stage{
		{Name: "analyze", Workers: 1, Run: func(ctx context.Context, in *pipe.Item) ([]*pipe.Item, error) {
			idx := in.Data.(int)
			b := plan.Boundaries[idx]
			frames, err := fingerprint.NormalizeVideo(ctx, r.InputPath, 0,
				fingerprint.TimeRange{StartMillis: b.StartMillis,
					DurationMs: b.EndMillis - b.StartMillis}, geo)
			if err != nil {
				return nil, err
			}
			pcm := make([]int16, 480) // fixed 10 ms canonical slice
			payload := &windowPayload{index: idx, frames: frames}
			size := int64(len(frames)*geo.Width*geo.Height + len(pcm)*2)
			return []*pipe.Item{{ID: in.ID, Bytes: size, Data: payload}}, nil
		}},
		{Name: "emit", Workers: 1, Run: func(ctx context.Context, in *pipe.Item) ([]*pipe.Item, error) {
			payload := in.Data.(*windowPayload)
			return nil, r.emitSegment(ctx, payload, container, plan, cp, startFrom,
				store, writer, deduper, geo, inputSum)
		}},
	}
	runner, err := pipe.NewRunner(inflightBudget, len(plan.Boundaries), stages)
	if err != nil {
		return pipe.Metrics{}, err
	}
	seeds := make([]*pipe.Item, len(plan.Boundaries))
	for i := range plan.Boundaries {
		seeds[i] = &pipe.Item{ID: "win", Bytes: 1, Data: i}
	}
	return runner.Run(ctx, seeds)
}

type windowPayload struct {
	index  int
	frames [][]byte
}

func (r *PipelineResult) emitSegment(ctx context.Context, payload *windowPayload,
	container *mp4.Container, plan *segment.Plan, cp *resume.Checkpoint,
	startFrom int, store *resume.Store, writer *segment.Writer,
	deduper *fingerprint.Deduper, geo fingerprint.FrameGeometry,
	inputSum string) error {

	idx := payload.index
	b := plan.Boundaries[idx]
	window := fingerprint.CanonicalWindow{
		SegmentID:  segmentName(idx),
		AudioRate:  audio.CanonicalRate,
		MonoPCM:    make([]int16, 480),
		Geometry:   geo,
		GrayFrames: payload.frames,
		ColorTags:  colorTags(container),
	}
	// Every window feeds the deduper, including segments skipped on resume,
	// so dedup_hits is stable across reruns rather than depending on how far
	// the interrupted run progressed.
	decision, err := deduper.Add(window)
	if err != nil {
		return err
	}
	if idx < startFrom {
		return nil // output already committed in a previous run
	}
	path, err := writer.Emit(ctx, r.InputPath, container, b)
	if err != nil {
		return err
	}
	sum, err := resume.HashFile(path)
	if err != nil {
		return err
	}
	state := resume.SegmentState{
		Index: idx, Output: filepath.Base(path), Sha256: sum,
		Fingerprint: decision.Fingerprint,
		StartMillis: b.StartMillis, EndMillis: b.EndMillis,
	}
	if decision.Duplicate {
		state.DuplicateOf = decision.CanonicalID
	}
	cp.Segments = append(cp.Segments, state)
	cp.InputSha256 = inputSum
	cp.InputPath = r.InputPath
	cp.Tracks = trackStates(container)
	return store.Save(cp)
}

func detectSilence(ctx context.Context, path string, c *mp4.Container) (int, error) {
	var audioTrack *mp4.Track
	for _, t := range c.Tracks {
		if t.Kind == mp4.KindAudio {
			audioTrack = t
			break
		}
	}
	if audioTrack == nil {
		return 0, nil
	}
	det, err := audio.AnalyzeSilence(ctx, path, audio.DecodeOptions{
		StreamIndex: 0, Channels: int(audioTrack.Channels),
		SourceRate: int(audioTrack.SampleRate), TargetRate: audio.CanonicalRate,
	})
	if err != nil {
		return 0, err
	}
	return det.TotalSilenceMillis(), nil
}

func colorTags(c *mp4.Container) [4]uint16 {
	for _, t := range c.Tracks {
		if t.Kind == mp4.KindVideo && t.Color != nil {
			return [4]uint16{
				t.Color.ColourPrimaries, t.Color.TransferCharacteristics,
				t.Color.MatrixCoefficients, boolToU16(t.Color.FullRange),
			}
		}
	}
	return [4]uint16{}
}

func boolToU16(b bool) uint16 {
	if b {
		return 1
	}
	return 0
}

func trackStates(c *mp4.Container) []resume.TrackState {
	out := make([]resume.TrackState, 0, len(c.Tracks))
	for _, t := range c.Tracks {
		out = append(out, resume.TrackState{
			ID: t.ID, Kind: int(t.Kind), Timescale: t.Timescale,
			Duration: t.Duration.Value, Channels: t.Channels, Rate: t.SampleRate,
		})
	}
	return out
}

func segmentName(index int) string {
	return filepath.Base(segmentFile(index))
}

func segmentFile(index int) string {
	const digits = 3
	name := []byte("seg-000.mp4")
	pos := len("seg-") + digits - 1
	for i := 0; i < digits; i++ {
		name[pos-i] = byte('0' + index%10)
		index /= 10
	}
	return string(name)
}

// nowMillis is indirected so timing tests stay deterministic if needed.
var nowMillis = func() int64 { return time.Now().UnixMilli() }
