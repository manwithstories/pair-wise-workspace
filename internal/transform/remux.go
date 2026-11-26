// Package transform re-wraps recovered tracks according to the composed
// timeline. It stream-copies edit-list segments (no re-encode), records the
// source offset of out-of-bounds edits and missing tracks, and always keeps
// processing the remaining independent tracks.
package transform

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"bq/internal/clock"
	"bq/internal/ffmpeg"
	"bq/internal/manifest"
	"bq/internal/model"
	"bq/internal/timeline"
)

// Outcome describes one stream's remux result.
type Outcome struct {
	StreamID    int
	OutputPath  string
	Skipped     bool
	Segments    int
	FirstOffset int64
}

// Remuxer runs the per-track transform inside one output directory.
type Remuxer struct {
	manifest *manifest.Manifest
	tl       *timeline.Timeline
	baseDir  string
}

// New builds a Remuxer. baseDir is the run output directory; remuxed clips
// are written under baseDir/remux and never outside it.
func New(m *manifest.Manifest, tl *timeline.Timeline, baseDir string) *Remuxer {
	return &Remuxer{manifest: m, tl: tl, baseDir: baseDir}
}

// Run processes every declared stream independently. A failure on one track
// is recorded and does not abort the others.
func (r *Remuxer) Run(ctx context.Context) ([]Outcome, []model.Error) {
	outDir := filepath.Join(r.baseDir, "remux")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, []model.Error{model.NewError(model.StageTransform, 0,
			model.ErrCorruptManifest, model.SeverityFatal,
			"cannot create remux dir: "+err.Error(), -1, 0, false)}
	}
	var outcomes []Outcome
	var errs []model.Error

	for i := range r.manifest.Streams {
		s := &r.manifest.Streams[i]
		oc := Outcome{StreamID: s.ID, FirstOffset: -1}
		srcPath := filepath.Join(r.baseDir, "media", filepath.Base(s.File))
		missing := false
		if _, err := os.Stat(srcPath); err != nil {
			missing = true
			errs = append(errs, model.NewError(model.StageTransform, s.ID,
				model.ErrMissingTrack, model.SeverityError,
				fmt.Sprintf("declared track file missing: %s", s.File), -1, 0, false))
			oc.Skipped = true
		}

		edits := r.tl.Edits[s.ID]
		if !missing && len(edits) == 0 {
			// No edit list: remux the whole available track as one segment.
			clip := filepath.Join(outDir, fmt.Sprintf("stream%d-full.mp4", s.ID))
			if err := ffmpeg.Remux(ctx, ffmpeg.RemuxOptions{
				InPath: srcPath, OutPath: clip,
				StartSeconds: 0, EndSeconds: float64(r.manifest.DurationS),
			}); err != nil {
				errs = append(errs, remuxError(s.ID, err, -1))
				oc.Skipped = true
			} else {
				oc.OutputPath = clip
				oc.Segments = 1
			}
			outcomes = append(outcomes, oc)
			continue
		}

		segments := 0
		for ei, ed := range edits {
			if missing {
				break
			}
			boundsErr := r.checkBounds(s, ed)
			if boundsErr != nil {
				errs = append(errs, *boundsErr)
				if oc.FirstOffset < 0 {
					oc.FirstOffset = boundsErr.SourceOffset
				}
				continue // skip this edit, keep processing independent segments
			}
			// Clamp the segment end to the actual recovered tail so short
			// fixtures with full-length edit lists still remux valid media.
			startM, endM := r.clampSegment(s.ID, ed)
			clip := filepath.Join(outDir, fmt.Sprintf("stream%d-seg%d.mp4", s.ID, ei))
			err := ffmpeg.Remux(ctx, ffmpeg.RemuxOptions{
				InPath: srcPath, OutPath: clip,
				StartSeconds: float64(startM) / float64(clock.MasterHz),
				EndSeconds:   float64(endM) / float64(clock.MasterHz),
			})
			if err != nil {
				errs = append(errs, remuxError(s.ID, err, ed.StartSource))
				continue
			}
			segments++
			if oc.OutputPath == "" {
				oc.OutputPath = clip
			}
		}
		oc.Segments = segments
		if segments == 0 && !missing {
			oc.Skipped = true
		}
		outcomes = append(outcomes, oc)
	}
	return outcomes, errs
}

// checkBounds verifies an edit segment against the timeline's actual items.
// Only an edit whose start lies past the recovered tail is truly out of
// bounds; segments merely extending beyond it are clamped at remux time.
func (r *Remuxer) checkBounds(s *manifest.Stream, ed timeline.EditSegment) *model.Error {
	items := r.tl.Streams[s.ID]
	if len(items) == 0 {
		e := model.NewError(model.StageTransform, s.ID, model.ErrMissingTrack,
			model.SeverityError, "no recovered items for stream", ed.StartSource, ed.StartSource, true)
		return &e
	}
	last := items[len(items)-1].MasterPTS
	if ed.StartMaster > last {
		e := model.NewError(model.StageTransform, s.ID, model.ErrEditOutOfBounds,
			model.SeverityError,
			fmt.Sprintf("edit start %d exceeds recovered tail %d", ed.StartMaster, last),
			ed.StartSource, ed.StartSource, true)
		return &e
	}
	return nil
}

// clampSegment limits a segment's end to the recovered tail while keeping
// the declared start. The start is guaranteed valid by checkBounds.
func (r *Remuxer) clampSegment(streamID int, ed timeline.EditSegment) (int64, int64) {
	items := r.tl.Streams[streamID]
	last := items[len(items)-1].MasterPTS
	end := ed.EndMaster
	if end > last {
		end = last
	}
	if end <= ed.StartMaster {
		end = ed.StartMaster + clock.MasterHz // at least one master second
	}
	return ed.StartMaster, end
}

func remuxError(streamID int, err error, sourcePTS int64) model.Error {
	hasPTS := sourcePTS >= 0
	return model.NewError(model.StageTransform, streamID, model.ErrEditOutOfBounds,
		model.SeverityError, "remux failed: "+err.Error(), sourcePTS, sourcePTS, hasPTS)
}
