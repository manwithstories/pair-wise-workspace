package syncer

import (
	"testing"

	"pairflow/internal/model"
)

func TestAlignVariableFrameRateAndAudioGrid(t *testing.T) {
	// Deliberately irregular intervals; alignment must use PTS, not a nominal
	// frame rate.
	pts := []int64{0, 16_666_667, 50_000_001, 66_666_668, 116_666_668}
	durations := []int64{16_666_667, 33_333_334, 16_666_667, 50_000_000, 40_000_000}
	var points []FramePoint
	for seq, p := range pts {
		points = append(points,
			FramePoint{TrackID: "cam-01", Sequence: int64(seq), PTSNS: p, DurationNS: durations[seq]},
			FramePoint{TrackID: "cam-02", Sequence: int64(seq), PTSNS: p + 250, DurationNS: durations[seq]},
		)
	}
	audio := AudioGrid{SampleRateHz: 48000}
	rep := Align(points, audio, nil)
	if rep.TrackCount != 2 || rep.AlignedFrames != 10 {
		t.Fatalf("tracks=%d frames=%d", rep.TrackCount, rep.AlignedFrames)
	}
	if rep.MaxSkewNS > 20_000 { // 0.5us inter-track + PCM quantization
		t.Fatalf("max skew too large: %dns (%dus)", rep.MaxSkewNS, rep.MaxSkewUS)
	}
	if rep.MaxSkewUS > 21 {
		t.Fatalf("reported microseconds too coarse: %d", rep.MaxSkewUS)
	}
}

func TestAlignPropagatesTransportGaps(t *testing.T) {
	gap := Gap{TrackID: "rtp", Reason: "burst_loss", Sequences: []uint16{811, 812}, Interval: model.Interval{StartNS: 1, EndNS: 2, StartSeq: 811, EndSeq: 812}}
	rep := Align([]FramePoint{{TrackID: "cam", Sequence: 1, PTSNS: 1000}}, AudioGrid{SampleRateHz: 48000}, []Gap{gap})
	if len(rep.Gaps) != 1 || len(rep.GapRanges) != 1 {
		t.Fatalf("gaps=%d ranges=%d", len(rep.Gaps), len(rep.GapRanges))
	}
	if rep.Gaps[0].Sequences[0] != 811 || rep.Gaps[0].Sequences[1] != 812 {
		t.Fatalf("unexpected gap sequences: %v", rep.Gaps[0].Sequences)
	}
}
