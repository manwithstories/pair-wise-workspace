package pipeline

import (
	"bytes"
	"context"

	"mediadeliver/internal/demux"
	"mediadeliver/internal/model"
)

// demuxMaterial unifies MP4 and RTP sources into packets plus track infos.
// Corruption surfaces as a classified model.*Error carrying its first
// position, ready to be isolated by the caller.
func (rn *Runner) demuxMaterial(src *MaterialSource) ([]*model.Packet, []model.TrackInfo, error) {
	ctx := context.Background()
	switch src.Format {
	case "mp4":
		r := demux.NewMP4Reader(src.Material.ID, bytes.NewReader(src.Data), rn.limits)
		infos, err := r.Probe()
		if err != nil {
			return nil, nil, err
		}
		pkts, err := r.ReadAll(ctx)
		return pkts, infos, err
	case "rtp":
		if len(src.Material.Tracks) != 1 {
			return nil, nil, model.At(model.KindInvalidInput, "pipeline", "demux rtp",
				int64(src.Material.Line), "an RTP material must declare exactly one track")
		}
		ts := src.Material.Tracks[0]
		r := demux.NewRTPReader(src.Material.ID, ts.ID, ts.Kind, ts.Codec,
			1, bytes.NewReader(src.Data), rn.limits)
		pkts, err := r.ReadAll(ctx)
		if err != nil {
			return nil, nil, err
		}
		info := r.TrackInfo()
		// RTP fixtures use a unit tick base (1 tick per packet); the manifest
		// supplies the real timescale when edits are present.
		return pkts, []model.TrackInfo{info}, nil
	default:
		return nil, nil, model.At(model.KindInvalidInput, "pipeline", "demux",
			int64(src.Material.Line), "unknown material format "+src.Format)
	}
}
