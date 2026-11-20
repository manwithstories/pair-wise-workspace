package testdata

import "testing"

func TestGenerateDeterministic(t *testing.T) {
	specs := DefaultTracks(500)
	cfg := Config{Seed: 7, Tracks: specs, Scenario: ScenarioReorderDup, SREvery: 64, BurstSize: 7}
	a := Generate(cfg)
	b := Generate(cfg)
	if len(a.Datagrams) != len(b.Datagrams) {
		t.Fatal("same seed produced different datagram counts")
	}
	for i := range a.Datagrams {
		if string(a.Datagrams[i]) != string(b.Datagrams[i]) {
			t.Fatalf("datagram %d differs across same-seed runs", i)
		}
	}
}

func TestGenerateScenarios(t *testing.T) {
	specs := DefaultTracks(300)
	cases := map[Scenario]func(g GroundTruth) bool{
		ScenarioClean:       func(g GroundTruth) bool { return g.Lost == 0 && g.Duplicates == 0 },
		ScenarioBurstLoss:   func(g GroundTruth) bool { return g.Lost > 0 },
		ScenarioReorderDup:  func(g GroundTruth) bool { return g.Duplicates > 0 },
		ScenarioTruncated:   func(g GroundTruth) bool { return g.Truncated > 0 },
		ScenarioCorruptRTCP: func(g GroundTruth) bool { return g.CorruptReports > 0 },
		ScenarioArchive:     func(g GroundTruth) bool { return g.Lost > 0 && g.Duplicates > 0 },
	}
	for sc, ok := range cases {
		s := Generate(Config{Seed: 1, Tracks: specs, Scenario: sc, SREvery: 64, BurstSize: 7})
		if !ok(s.Truth) {
			t.Errorf("scenario %s did not produce expected impairment: %+v", sc, s.Truth)
		}
	}
}

func TestGenerateEmitsReports(t *testing.T) {
	specs := DefaultTracks(100)
	s := Generate(Config{Seed: 1, Tracks: specs, Scenario: ScenarioClean, SREvery: 50, BurstSize: 7})
	if s.Truth.ValidReports == 0 {
		t.Error("expected valid RTCP sender reports")
	}
}

func TestGroundTruthKeyframes(t *testing.T) {
	specs := DefaultTracks(200) // video keyframe every 25
	s := Generate(Config{Seed: 1, Tracks: specs, Scenario: ScenarioClean, SREvery: 64})
	if s.Truth.Keyframes == 0 {
		t.Error("expected keyframes in ground truth")
	}
}
