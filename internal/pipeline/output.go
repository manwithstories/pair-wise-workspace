package pipeline

import (
	"fmt"
	"path/filepath"

	"github.com/pairwise/relay/internal/checkpoint"
	"github.com/pairwise/relay/internal/manifest"
	"github.com/pairwise/relay/internal/media"
	"github.com/pairwise/relay/internal/timeline"
)

type outputResult struct {
	report    *timeline.Report
	bundle    *manifest.Bundle
	artifacts []checkpoint.Artifact
	sha       string
}

func buildAndWriteOutputs(in Input, workDir string, trs []*resolvedRung, store *segmentStore) (*outputResult, error) {
	tracks := make([]timeline.TrackInput, 0, len(trs))
	for _, rr := range trs {
		track := timeline.TrackInput{Rendition: rr.input.ID, Timescale: rr.input.Timescale}
		for seg := uint64(1); seg <= uint64(in.Segments); seg++ {
			if f := store.parsed[rr.input.ID][seg]; f != nil {
				track.Fragments = append(track.Fragments, timeline.FragmentAt{Segment: seg, Frag: f})
			} else {
				track.Fragments = append(track.Fragments, timeline.FragmentAt{Segment: seg, Gap: true})
			}
		}
		tracks = append(tracks, track)
	}
	report, err := timeline.Align(tracks, in.Segments, in.Duration90k)
	if err != nil {
		return nil, err
	}
	if report.Aligned != in.Segments {
		return nil, media.NewExitError(3, media.NewFault(media.CodeUnrecoverable, media.FragmentID{}, -1, false,
			"only %d/%d common boundaries aligned", report.Aligned, in.Segments))
	}

	renditions := make([]manifest.Rendition, 0, len(trs))
	var artifacts []checkpoint.Artifact
	for ri, rr := range trs {
		id := rr.input.ID
		mr := manifest.Rendition{
			ID: id, Playlist: id + ".m3u8", Bandwidth: rr.input.Bandwidth, Average: rr.input.Average,
			Codecs: rr.input.Codecs, Resolution: rr.input.Resolution, FrameRate: rr.input.FrameRate,
		}
		for _, b := range report.Boundaries {
			var cell timeline.Cell
			for _, c := range b.Cells {
				if c.Rendition == id {
					cell = c
					break
				}
			}
			uri := fmt.Sprintf("media/%s/seg-%05d.m4s", id, b.Index)
			mr.Segments = append(mr.Segments, manifest.Segment{
				Index: b.Index, URI: uri, Start90k: b.Start90k, Duration90k: b.Duration90k,
				Discontinuity: b.Discontinuity, Gap: cell.Gap,
			})

			if !cell.Gap && store.parsed[id][b.Index] != nil {
				data := rr.data[b.Index]
				if data == nil {
					return nil, media.NewExitError(3, media.NewFault(media.CodeUnrecoverable,
						media.FragmentID{Rendition: id, Segment: b.Index}, -1, false, "aligned segment has no media bytes"))
				}
				rel := filepath.ToSlash(filepath.Join("media", id, fmt.Sprintf("seg-%05d.m4s", b.Index)))
				full, err := safeJoin(workDir, rel)
				if err != nil {
					return nil, err
				}
				if err := writeAtomic(full, data, 0o640); err != nil {
					return nil, err
				}
				sum, size, err := checkpoint.HashFile(full)
				if err != nil {
					return nil, err
				}
				artifacts = append(artifacts, checkpoint.Artifact{Path: rel, SHA256: sum, Size: size})
			}
		}
		renditions = append(renditions, mr)
		_ = ri
	}

	bundle, err := manifest.Build("master.m3u8", renditions)
	if err != nil {
		return nil, err
	}
	for _, name := range bundle.SortedNames() {
		data := bundle.Files[name]
		full, err := safeJoin(workDir, name)
		if err != nil {
			return nil, err
		}
		if err := writeAtomic(full, data, 0o640); err != nil {
			return nil, err
		}
		sum, size, err := checkpoint.HashFile(full)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, checkpoint.Artifact{Path: name, SHA256: sum, Size: size})
	}
	sha, err := checkpoint.HashArtifactSet(artifacts)
	if err != nil {
		return nil, err
	}
	return &outputResult{report: report, bundle: bundle, artifacts: artifacts, sha: sha}, nil
}
