package pairflow_test

import (
	"testing"

	"pairflow/internal/syncer"
)

func TestSyncerFileContract(t *testing.T) {
	points := []syncer.FramePoint{
		{TrackID: "a", Sequence: 1, PTSNS: 1_000_000},
		{TrackID: "b", Sequence: 1, PTSNS: 1_000_500},
	}
	rep := syncer.Align(points, syncer.AudioGrid{SampleRateHz: 48000}, nil)
	if rep.AlignedFrames != 2 || rep.MaxSkewNS != 500 {
		t.Fatalf("report=%+v", rep)
	}
	if rep.MaxSkewUS != 1 {
		t.Fatalf("ceil skew us=%d want 1", rep.MaxSkewUS)
	}
}
