package cut

import (
	"testing"

	"fieldcut/internal/clock"
)

func TestPlanCutsLegalBoundariesAndCoverage(t *testing.T) {
	errCross := &clock.Error{Code: clock.CodeBadInterval, TrackID: 4, StartNs: 1500 * clock.NsPerMs, EndNs: 1600 * clock.NsPerMs}
	plan, err := PlanCuts(Request{
		TrackID: 1, Duration: 4 * clock.NsPerSecond, AudioSampleRate: 48000, VideoFPS: 30,
		Candidates: []Candidate{
			{Time: clock.NsPerSecond, Kind: CandidateChapter},
			{Time: 2 * clock.NsPerSecond, Kind: CandidateSilence},
			{Time: 3 * clock.NsPerSecond, Kind: CandidateRandomAccess},
		},
		Errors: []*clock.Error{errCross},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Segments) != 6 {
		t.Fatalf("segments=%v points=%v", plan.Segments, plan.Points)
	}
	for _, p := range plan.Points {
		_, rounded, err := clock.RoundToSample(p.Ns, 48000)
		if err != nil {
			t.Fatal(err)
		}
		if rounded != p.Ns {
			t.Fatalf("boundary %d is not a rounded 48k sample (%d)", p.Ns, rounded)
		}
	}
	if plan.MaxSkewNs > clock.NsPerMs {
		t.Fatalf("skew=%d", plan.MaxSkewNs)
	}
	if err := Coverage(plan.Segments, 4*clock.NsPerSecond); err != nil {
		t.Fatal(err)
	}
	if len(plan.Errors) != 1 || len(plan.ErrorAnchorNs) != 2 {
		t.Fatalf("cross-track error not propagated: errors=%d anchors=%v", len(plan.Errors), plan.ErrorAnchorNs)
	}
}

func TestSplitSamplesHalfOpen(t *testing.T) {
	segs := []clock.Interval{{0, 10}, {10, 20}}
	samples := []TimedSample{{PTS: 0, Data: []byte("a")}, {PTS: 9, Data: []byte("b")}, {PTS: 10, Data: []byte("c")}}
	parts, counts := SplitSamples(samples, segs)
	if counts[0] != 2 || counts[1] != 1 || string(parts[1][0].Data) != "c" {
		t.Fatalf("parts=%v counts=%v", parts, counts)
	}
}

func TestCoverageRejectsGap(t *testing.T) {
	err := Coverage([]clock.Interval{{0, 10}, {11, 20}}, 20)
	if err == nil {
		t.Fatal("expected gap error")
	}
	if ce, ok := clock.AsError(err); !ok || ce.Code != clock.CodeCoverage {
		t.Fatalf("unexpected %v", err)
	}
}
