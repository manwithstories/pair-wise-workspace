package sim

import (
	"testing"
)

func TestTraceDeterministicAcrossBuilds(t *testing.T) {
	for _, sc := range []string{ScenarioNominal, ScenarioCorrupt, ScenarioRecovery, ScenarioBurst} {
		a, err := Build(sc, 41, DefaultParams())
		if err != nil {
			t.Fatal(err)
		}
		b, err := Build(sc, 41, DefaultParams())
		if err != nil {
			t.Fatal(err)
		}
		if len(a.Actions) != len(b.Actions) {
			t.Fatalf("%s: action count differs across builds", sc)
		}
		for i := range a.Actions {
			if a.Actions[i] != b.Actions[i] {
				t.Fatalf("%s: action %d differs across builds: %+v vs %+v", sc, i, a.Actions[i], b.Actions[i])
			}
		}
		// A different seed must change at least something.
		c, _ := Build(sc, 42, DefaultParams())
		same := true
		for i := range a.Actions {
			if i >= len(c.Actions) || a.Actions[i] != c.Actions[i] {
				same = false
				break
			}
		}
		if same && len(a.Actions) == len(c.Actions) {
			t.Fatalf("%s: seed change had no effect", sc)
		}
	}
}

func TestScenarioImpairments(t *testing.T) {
	nominal, _ := Build(ScenarioNominal, 7, DefaultParams())
	for _, ac := range nominal.Actions {
		if ac.Kind != ActDeliver {
			t.Fatal("nominal trace must contain only deliveries")
		}
	}
	corrupt, _ := Build(ScenarioCorrupt, 7, DefaultParams())
	if len(corrupt.LostSeqs) == 0 {
		t.Fatal("corrupt trace must lose packets")
	}
	hasDup, hasReorder := false, false
	for _, ac := range corrupt.Actions {
		if ac.Kind == ActDuplicate {
			hasDup = true
		}
		if ac.Kind == ActReorder {
			hasReorder = true
		}
	}
	if !hasDup || !hasReorder {
		t.Fatalf("corrupt trace needs dupes and reorders dup=%v reorder=%v", hasDup, hasReorder)
	}
	recovery, _ := Build(ScenarioRecovery, 7, DefaultParams())
	if len(recovery.BurstRanges) == 0 {
		t.Fatal("recovery trace needs bracketed loss runs")
	}
	burst, _ := Build(ScenarioBurst, 7, DefaultParams())
	maxRun := 0
	for _, r := range burst.BurstRanges {
		if run := int(r[1]-r[0]) + 1; run > maxRun {
			maxRun = run
		}
	}
	if maxRun < 5 {
		t.Fatalf("burst trace needs an unrecoverable-scale burst, max run %d", maxRun)
	}
}

func TestArrivalOrderAndUnknownScenario(t *testing.T) {
	tr, _ := Build(ScenarioBurst, 3, DefaultParams())
	for i := 1; i < len(tr.Actions); i++ {
		if tr.Actions[i].Arrival < tr.Actions[i-1].Arrival {
			t.Fatal("actions must be emitted in arrival order")
		}
	}
	if _, err := Build("nope", 1, DefaultParams()); err == nil {
		t.Fatal("unknown scenario must error")
	}
}

func TestVideoFaultsAndDeterminism(t *testing.T) {
	clean := BuildVideo(ScenarioNominal, 41, DefaultVideoParams())
	for _, f := range clean.Frames {
		if f.PTS <= 0 && f.FrameID != 0 {
			t.Fatal("nominal video must keep every timestamp")
		}
	}
	corrupt := BuildVideo(ScenarioCorrupt, 41, DefaultVideoParams())
	if len(corrupt.MissingPTS) == 0 || len(corrupt.RegressivePTS) == 0 {
		t.Fatal("corrupt video must include missing and regressive timestamps")
	}
	again := BuildVideo(ScenarioCorrupt, 41, DefaultVideoParams())
	for i := range corrupt.Frames {
		if corrupt.Frames[i] != again.Frames[i] {
			t.Fatal("video fixtures must be reproducible for the same seed")
		}
	}
}
