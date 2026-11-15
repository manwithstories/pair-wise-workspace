package timeline

import (
	"math/big"
	"testing"

	"dashkit/internal/model"
)

func u64(v uint64) *uint64 { return &v }

func mkMPD(offsets ...uint64) *model.MPD {
	m := &model.MPD{Periods: []*model.Period{{Start: big.NewRat(0, 1)}}}
	as := &model.AdaptationSet{ContentType: "video"}
	ids := []string{"a", "b", "c"}
	for i, off := range offsets {
		as.Reps = append(as.Reps, &model.Representation{ID: ids[i], Bandwidth: 1,
			Template: &model.SegmentTemplate{
				Timescale: 90000, PresentationTimeOffset: new(big.Rat), StartNumber: 1,
				Timeline: &model.SegmentTimeline{S: []*model.S{{T: u64(off), D: 90000, R: 9}}},
			}})
	}
	m.Periods[0].Sets = []*model.AdaptationSet{as}
	return m
}

func TestAlignClean(t *testing.T) {
	al, probs := Align(mkMPD(0, 0, 0))
	if len(probs) != 0 || al.Mismatches != 0 || al.MaxDeviationMS.Sign() != 0 ||
		al.SwitchPoints != 10 || al.PTODriftTicks.Sign() != 0 {
		t.Fatalf("clean align: %+v probs=%v", al, probs)
	}
}

func TestAlignOffset(t *testing.T) {
	// rep c offset by 450 ticks = 5ms at timescale 90000
	al, _ := Align(mkMPD(0, 0, 450))
	if al.Mismatches != 10 {
		t.Fatalf("mismatches=%d want 10", al.Mismatches)
	}
	if al.MaxDeviationMS.Cmp(big.NewRat(5, 1)) != 0 {
		t.Fatalf("max deviation %s want 5", al.MaxDeviationMS.RatString())
	}
	if len(al.Worst) != 3 || al.Worst[0].RepID != "c" {
		t.Fatalf("worst: %+v", al.Worst)
	}
}

func TestAlignPTODrift(t *testing.T) {
	m := mkMPD(0, 0, 0)
	m.Periods[0].Sets[0].Reps[2].Template.PresentationTimeOffset = big.NewRat(3003, 1)
	al, probs := Align(m)
	if al.PTODriftTicks.Cmp(big.NewRat(3003, 1)) != 0 {
		t.Fatalf("drift %s", al.PTODriftTicks.RatString())
	}
	found := false
	for _, p := range probs {
		if p.Code == model.CodeNegativeTime {
			found = true
		}
	}
	if !found {
		t.Fatal("expected E_NEGATIVE_TIME")
	}
}
