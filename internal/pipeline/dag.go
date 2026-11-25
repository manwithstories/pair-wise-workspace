package pipeline

import (
	"fmt"
	"strconv"

	"github.com/pairwise/mediad/internal/fixture"
	"github.com/pairwise/mediad/internal/flow"
	"github.com/pairwise/mediad/internal/media"
	"github.com/pairwise/mediad/internal/metric"
	"github.com/pairwise/mediad/internal/repair"
)

func initNodeID(trackID string) string {
	return fmt.Sprintf("repair:%s:init-segment", trackID)
}

func segmentNodeID(trackID string, segmentInTrack int) string {
	return fmt.Sprintf("repair:%s:segment-%02d", trackID, segmentInTrack)
}

func buildDAG(b *fixture.Bundle, report *repair.Report) (*flow.DAG, error) {
	d := flow.New()
	for _, track := range b.Tracks {
		track := track
		id := initNodeID(track.ID)
		init := track.Init
		err := d.Add(&flow.Node{
			ID:        id,
			OutputRel: fmt.Sprintf("init/%s.mp4", track.ID),
			Inputs: map[string]string{
				"seed":      strconv.FormatInt(b.Seed, 10),
				"track":     track.ID,
				"timescale": strconv.FormatUint(uint64(track.Timescale), 10),
				"init_sha":  metric.BytesSHA256(init),
			},
			Retries: 1,
			Produce: func() ([]byte, error) {
				if init == nil {
					return nil, media.NewError(media.CodeBlockedDependency, "repair.init", id, "initialization segment is missing")
				}
				if err := repair.ValidateBoxes(init); err != nil {
					return nil, err
				}
				return init, nil
			},
		})
		if err != nil {
			return nil, err
		}
	}

	for _, seg := range report.Segments {
		seg := seg
		err := d.Add(&flow.Node{
			ID:        segmentNodeID(seg.TrackID, seg.SegmentInTrack),
			Deps:      []string{initNodeID(seg.TrackID)},
			OutputRel: seg.OutputRel,
			Retries:   1,
			Inputs: map[string]string{
				"seed":          strconv.FormatInt(b.Seed, 10),
				"global_index":  strconv.Itoa(seg.GlobalIndex),
				"start_ticks":   strconv.FormatInt(seg.StartTicks, 10),
				"end_ticks":     strconv.FormatInt(seg.EndTicks, 10),
				"tfdt_out":      strconv.FormatUint(seg.TFDTOut, 10),
				"keyframe":      strconv.FormatBool(seg.Keyframe),
				"complete":      strconv.FormatBool(seg.Complete),
				"fragment_sha":  metric.BytesSHA256(seg.Data),
			},
			Produce: func() ([]byte, error) {
				if !seg.Complete {
					return nil, media.NewError(media.CodeUnrepairedGap, "repair.align", seg.OutputRel, "bounded window expired with missing packets")
				}
				if err := repair.ValidateBoxes(seg.Data); err != nil {
					return nil, err
				}
				return seg.Data, nil
			},
		})
		if err != nil {
			return nil, err
		}
	}
	return d, nil
}
