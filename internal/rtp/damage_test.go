package rtp

import (
	"testing"

	"mediad/internal/model"
)

func TestDamagedRangesMarkSamples(t *testing.T) {
	tr := model.Track{ID: 1, Kind: model.Video, Timescale: 90000}
	for i := 0; i < 4; i++ {
		tr.Samples = append(tr.Samples, model.Sample{
			Index: i, DTS: mustRat(int64(i)*3000, 90000), Duration: mustRat(3000, 90000),
		})
	}
	rep := LossReport{Gaps: []Gap{{StartTicks: 3000, EndTicks: 3000, Recoverable: false}}}
	if err := DamagedRanges(&tr, rep); err != nil {
		t.Fatal(err)
	}
	if !tr.Samples[1].Damaged {
		t.Fatal("sample 1 should be damaged")
	}
	if tr.Samples[2].Damaged {
		t.Fatal("sample 2 should be intact")
	}
}
