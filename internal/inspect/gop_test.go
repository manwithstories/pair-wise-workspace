package inspect

import (
	"testing"

	"mediaflow/internal/clock"
	"mediaflow/internal/cmaf"
)

func TestGOPAccumulatorCadence(t *testing.T) {
	g := NewGOPAccumulator(1, 1000)
	for i := 0; i <= 5; i++ {
		for f := 0; f < 25; f++ {
			us := int64(i*1000+f*40) * 1000
			g.Observe(us, f == 0, true)
		}
	}
	st := g.Result()
	if st.Keyframes != 6 {
		t.Fatalf("keyframes = %d", st.Keyframes)
	}
	if len(st.Intervals) != 5 {
		t.Fatalf("intervals = %v", st.Intervals)
	}
	if st.MinMS != 1000 || st.MaxMS != 1000 || st.MeanMS != 1000 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestGOPUncertainNotSilentlyFixed(t *testing.T) {
	g := NewGOPAccumulator(1, 1000)
	g.Observe(0, true, true)
	g.Observe(40_000, false, true)
	g.Observe(80_000, true, false)
	g.Observe(500_000, false, true)
	st := g.Result()
	if st.Uncertain != 1 || len(st.Intervals) != 0 || st.Keyframes != 2 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestKeyframeIssueDetection(t *testing.T) {
	s := cmaf.Sample{TrackID: 1, Index: 4, Offset: 100, DurationTicks: 0, IsKeyframe: true}
	if issue := DetectKeyframeIssues(1, s); issue == nil || issue.Kind != clock.ErrKeyframeConflict {
		t.Fatalf("zero-duration sync not flagged: %+v", issue)
	}
	s = cmaf.Sample{TrackID: 1, Index: 5, FlagsConflict: true, IsKeyframe: true}
	if issue := DetectKeyframeIssues(1, s); issue == nil {
		t.Fatal("flag conflict not flagged")
	}
	s = cmaf.Sample{TrackID: 1, Index: 6, DurationTicks: 3000, IsKeyframe: true}
	if issue := DetectKeyframeIssues(1, s); issue != nil {
		t.Fatalf("clean sample flagged: %+v", issue)
	}
}
