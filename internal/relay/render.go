package relay

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/model"
	"relay.local/relay/internal/pipeline"
	"relay.local/relay/internal/render"
	"relay.local/relay/internal/scene"
	"relay.local/relay/internal/timeline"
)

// renderAll renders every composed video frame into dir and returns the PNG
// digests in canonical order. cancelAfter > 0 stops the producer after that
// many frames and leaves a checkpoint for a later resume.
func renderAll(ctx context.Context, sc *scene.Scenario, opts Options, dir string, cancelAfter int, anchors []timeline.Anchor) ([]string, error) {
	entries, mp4Track, desc, err := composeFrames(sc)
	if err != nil {
		return nil, err
	}
	// Resume: skip frames a previous cancelled run already published. The
	// checkpoint records how many frames were written and each existing file
	// is hashed so the final manifest matches an uninterrupted run exactly.
	var hashes []string
	resumeFrom := 0
	if opts.Resume {
		done, err := readCheckpoint(filepath.Join(dir, "checkpoint.json"))
		if err != nil {
			return nil, err
		}
		if done > len(entries) {
			return nil, fault.New(-1, 0, fault.KindCorrupt, false,
				"checkpoint claims %d frames but the timeline has %d", done, len(entries))
		}
		for i := 0; i < done; i++ {
			h, err := hashExistingFrame(dir, entries[i].SampleIdx)
			if err != nil {
				return nil, err
			}
			hashes = append(hashes, h)
		}
		resumeFrom = done
		entries = entries[resumeFrom:]
	}
	cctx := ctx
	if cancelAfter > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithCancel(ctx)
		defer cancel()
	}
	var processed int
	res, runErr := pipeline.Run(cctx, pipeline.Config{Workers: opts.Workers, Inflight: opts.Inflight}, pipeline.RunOptions{
		Items: entries,
		Work: func(c context.Context, e timeline.Entry) (any, error) {
			if c.Err() != nil {
				return nil, fault.Wrap(-1, 0, fault.KindCancelled, true, c.Err())
			}
			sample, ok := mp4Track.SampleByIndex(e.SampleIdx)
			if !ok {
				return nil, fault.New(mp4Track.Number, 0, fault.KindOutOfRange, true,
					"sample %d missing", e.SampleIdx)
			}
			payload, err := mp4Track.Payload(sc.MP4.File.Data, sample)
			if err != nil {
				return nil, fault.Wrap(mp4Track.Number, sample.Offset, fault.KindOutOfRange, true, err)
			}
			// Workers decode and crop in memory only; the write happens in the
			// serialized emission stage so cancellation before a write means no
			// file is created at all.
			r := render.RenderFrame(e.SampleIdx, payload, sample.Codec, desc, render.Options{})
			if r.Err != nil {
				return nil, r.Err
			}
			return r, nil
		},
		OnResult: func(m pipeline.Merged) error {
			if cancelAfter > 0 && processed >= cancelAfter {
				return fault.New(-1, 0, fault.KindCancelled, true, "simulated cancellation")
			}
			r, ok := m.Value.(render.Result)
			if !ok {
				return fault.New(-1, 0, fault.KindCorrupt, false, "unexpected job value %T", m.Value)
			}
			written, err := render.WriteRendered(dir, r)
			if err != nil {
				return err
			}
			processed++
			hashes = append(hashes, written.OutHash)
			return nil
		},
	})
	if isCancel(runErr) || res.Canceled {
		writeCheckpoint(dir, resumeFrom+processed, resumeFrom+len(entries))
		return nil, fault.New(-1, 0, fault.KindCancelled, true,
			"cancelled after %d frames", resumeFrom+processed)
	}
	if runErr != nil {
		return nil, runErr
	}
	if processed != len(entries) {
		return nil, fault.New(-1, 0, fault.KindCorrupt, false,
			"rendered %d of %d frames", processed, len(entries))
	}
	// A completed run has nothing left to resume.
	_ = os.Remove(filepath.Join(dir, "checkpoint.json"))
	// The pipeline emits in canonical order, so hashes are already ordered by
	// composition key; sort defensively by frame index if the merge changed.
	return hashes, nil
}

// composeFrames composes the MP4 video track on the shared timeline.
func composeFrames(sc *scene.Scenario) ([]timeline.Entry, *model.Track, model.Description, error) {
	mp4Track, ok := scene.VideoTrack(sc.MP4)
	if !ok {
		return nil, nil, model.Description{}, fault.New(-1, 0, fault.KindCorrupt, false, "no MP4 video track")
	}
	comp := timeline.Compose([]timeline.TrackSource{{Track: mp4Track}},
		timeline.ComposeOptions{MaxEntries: 1 << 16})
	if len(comp.Entries) == 0 {
		return nil, nil, model.Description{}, fault.New(-1, 0, fault.KindCorrupt, false,
			"composition produced no frames")
	}
	entries := make([]timeline.Entry, 0, len(comp.Entries))
	for _, e := range comp.Entries {
		if e.Codec == model.CodecMJPEG {
			entries = append(entries, e)
		}
	}
	return entries, mp4Track, mp4Track.Resolved, nil
}

func isCancel(err error) bool {
	fe, ok := fault.As(err)
	return ok && fe.Kind == fault.KindCancelled
}

// writeCheckpoint records progress so a resumed run can start where the
// cancelled one stopped.
func writeCheckpoint(dir string, done, total int) {
	body := fmt.Sprintf("{\"done\":%d,\"total\":%d}\n", done, total)
	_ = os.WriteFile(filepath.Join(dir, "checkpoint.json"), []byte(body), 0o644)
}

// printReport renders the stable key=value summary. The acceptance fields come
// first in a fixed order so the output can be matched literally.

// readCheckpoint parses the tiny JSON checkpoint written by writeCheckpoint.
func readCheckpoint(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fault.New(-1, 0, fault.KindCorrupt, true,
				"--resume requested but no checkpoint exists at %s", path)
		}
		return 0, fault.Wrap(-1, 0, fault.KindCorrupt, false, err)
	}
	var cp struct {
		Done  int `json:"done"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(data, &cp); err != nil {
		return 0, fault.Wrap(-1, 0, fault.KindCorrupt, false, err)
	}
	if cp.Done < 0 || cp.Done > cp.Total {
		return 0, fault.New(-1, 0, fault.KindCorrupt, false,
			"checkpoint progress %d/%d is inconsistent", cp.Done, cp.Total)
	}
	return cp.Done, nil
}

// hashExistingFrame recomputes a published frame's digest so a resumed run's
// manifest is identical to the uninterrupted one.
func hashExistingFrame(dir string, frameNo int) (string, error) {
	path := filepath.Join(dir, fmt.Sprintf("frame_%04d.png", frameNo))
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fault.Wrap(-1, 0, fault.KindCorrupt, false, err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum), nil
}
