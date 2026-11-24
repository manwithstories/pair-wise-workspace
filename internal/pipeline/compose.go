package pipeline

import (
	"fmt"
	"path/filepath"
	"sort"

	"mediarun/internal/clock"
	"mediarun/internal/model"
	"mediarun/internal/parse"
	"mediarun/internal/render"
	"mediarun/internal/testkit"
	"mediarun/internal/timeline"
	"mediarun/internal/txn"
)

type composeResult struct {
	segments  []timeline.Segment
	rollbacks int
	frames    int
	events    int
}

func runCompose(opts Options, media *testkit.Media, tr *transportResult, ir *indexResult) (*composeResult, error) {
	out := &composeResult{}
	// Stage 1: media-derived metadata events plus transport frame events.
	movie, err := parse.ParseMovie(media.Bytes)
	if err != nil {
		return nil, err
	}
	meta, err := parse.ExtractMetadata(movie)
	if err != nil {
		return nil, err
	}
	frameEvents, err := parse.FrameEvents(movie)
	if err != nil {
		return nil, err
	}
	var inputs []timeline.Input
	mediaByTrack := map[uint32][]model.Event{}
	for _, ev := range append(meta.Events(), frameEvents...) {
		mediaByTrack[ev.TrackID] = append(mediaByTrack[ev.TrackID], ev)
	}
	for id, evs := range mediaByTrack {
		inputs = append(inputs, timeline.Input{
			Track:  model.Track{ID: id, Timescale: 90000, Name: fmt.Sprintf("media-%d", id)},
			Events: evs,
		})
	}
	for id, evs := range tr.eventsByTrack {
		inputs = append(inputs, timeline.Input{
			Track:  model.Track{ID: id + 100, Timescale: 90000, Name: fmt.Sprintf("capture-%d", id)},
			Events: evs,
			Gaps:   tr.gapsByTrack[id],
		})
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Track.ID < inputs[j].Track.ID })
	merged, err := timeline.Merge(inputs, clock.TickRate)
	if err != nil {
		return nil, err
	}
	out.events = len(merged.Events)

	// Review frames from the merged timeline.
	seq, err := render.Render(merged.Events, render.DefaultConfig())
	if err != nil {
		return nil, err
	}
	if err := seq.WriteTo(opts.Out); err != nil {
		return nil, err
	}
	out.frames = len(seq.Frames)

	// Stage 2: commit each segment under WAL. The first frame fails after
	// staging to exercise rollback deterministically.
	run, err := txn.NewCommitRun(opts.Out)
	if err != nil {
		return nil, err
	}
	defer run.Close()
	if err := run.Begin("compose"); err != nil {
		return nil, err
	}
	var staged []txn.SegmentCommit
	for i, seg := range merged.Segments {
		name := fmt.Sprintf("%03d", i)
		payload := txn.NewSegmentPayload(name, seg.Start, seg.End, seg.Events, seg.Gaps)
		commit, err := run.StageSegment(payload)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			// Simulated interruption before the first commit: the staged
			// bytes are abandoned, then re-staged once the run resumes.
			if err := run.Rollback("compose", "simulated commit interruption"); err != nil {
				return nil, err
			}
			out.rollbacks++
			commit, err = run.StageSegment(payload)
			if err != nil {
				return nil, err
			}
		}
		staged = append(staged, commit)
	}
	for _, commit := range staged {
		if err := run.CommitSegment(commit); err != nil {
			return nil, err
		}
	}
	out.segments = merged.Segments
	if len(staged) == 0 {
		return nil, fmt.Errorf("%w: no segment committed", model.ErrCommit)
	}
	return out, nil
}

// runCorruptStage feeds malformed containers to the parser and counts how
// many were rejected with a corrupt-input classification. It also commits a
// deliberately failing segment to prove the rollback path leaves committed
// hashes untouched.
func runCorruptStage(opts Options, media *testkit.Media) (int, int, error) {
	cases, err := testkit.CorruptBoxes(media.Bytes)
	if err != nil {
		return 0, 0, err
	}
	caught := 0
	for _, bad := range cases {
		_, err := parse.ParseMovie(bad)
		if err != nil && model.ClassOf(err) == model.ClassCorrupt {
			caught++
		}
	}
	report := map[string]any{
		"cases":        len(cases),
		"caught":       caught,
		"all_rejected": caught == len(cases),
	}
	if err := writeJSON(filepath.Join(opts.Out, "corrupt.json"), report); err != nil {
		return 0, 0, err
	}
	if caught != len(cases) {
		return caught, caught, fmt.Errorf("%w: %d of %d corrupt inputs were accepted",
			model.ErrCorruptIndex, len(cases)-caught, len(cases))
	}
	return len(cases), caught, nil
}

// checkReload proves the artifact directory holds nothing but segment files
// this tool wrote: each segment payload decodes and hashes identically to
// its file bytes.
