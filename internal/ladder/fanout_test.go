package ladder

import (
	"testing"

	"mediad/internal/media"
)

func testVideo() *media.Track {
	tr := &media.Track{ID: 1, Kind: media.KindVideo, Timescale: 90_000}
	for i := uint32(0); i < 12; i++ {
		tr.Samples = append(tr.Samples, media.Sample{
			Index: i + 1, PTS: int64(i) * 3000, DTS: int64(i) * 3000, Duration: 3000,
			Sync: i%4 == 0,
		})
	}
	return tr
}

func TestBuildLadderSharesAllKeyframeBoundaries(t *testing.T) {
	l, faults := Build(testVideo(), DefaultLevels()[:2])
	if len(faults) != 0 {
		t.Fatalf("faults=%#v", faults)
	}
	if len(l.Renditions) != 2 {
		t.Fatalf("renditions=%d", len(l.Renditions))
	}
	if len(l.Renditions[0].Boundaries) != 3 {
		t.Fatalf("boundaries=%d", len(l.Renditions[0].Boundaries))
	}
	if l.MismatchCount() != 0 {
		t.Fatalf("mismatches=%d", l.MismatchCount())
	}
	res, f := l.CheckSwitch(0, 1, 5)
	if f != nil || !res.Aligned || res.DeltaNS != 0 {
		t.Fatalf("switch res=%#v fault=%v", res, f)
	}
}

func TestCheckSwitchRejectsNonKeyframe(t *testing.T) {
	l, _ := Build(testVideo(), nil)
	if _, f := l.CheckSwitch(0, 1, 2); f == nil {
		t.Fatal("expected non-keyframe switch failure")
	}
}

func TestBuildWithoutSyncFails(t *testing.T) {
	tr := testVideo()
	for i := range tr.Samples {
		tr.Samples[i].Sync = false
	}
	if _, faults := Build(tr, nil); len(faults) == 0 {
		t.Fatal("expected missing boundary fault")
	}
}
