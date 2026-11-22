package recovery

import (
	"testing"

	"example.com/medialab/archive/internal/model"
)

func TestDecideBoundedGapIsSilenceFill(t *testing.T) {
	d := Decide(Evidence{
		HoleSeq: 4, HoleStartUS: 1000, PreviousEndUS: 900,
		NextStartUS: 4000, HasNext: true, MaxFillUS: 1_000_000,
		Reason: model.KindUnknownGap,
	})
	if !d.Recoverable || d.DurationUS != 3000 || d.StartUS != 1000 || !d.Estimated {
		t.Fatalf("decision = %+v", d)
	}
	if d.Action != ActionSilenceFill {
		t.Fatalf("action = %q", d.Action)
	}
}

func TestDecideUsesPreviousEndWhenAfterHoleStart(t *testing.T) {
	d := Decide(Evidence{HoleStartUS: 500, PreviousEndUS: 800, NextStartUS: 1200, HasNext: true})
	if !d.Recoverable || d.StartUS != 800 || d.DurationUS != 400 {
		t.Fatalf("decision = %+v", d)
	}
}

func TestDecideUnrecoverableCases(t *testing.T) {
	cases := []Evidence{
		{HoleStartUS: 0, HasNext: false},
		{HoleStartUS: 1000, NextStartUS: 1000, HasNext: true},
		{HoleStartUS: 0, NextStartUS: 5_000_000, HasNext: true, MaxFillUS: 1_000_000},
	}
	for i, e := range cases {
		d := Decide(e)
		if d.Recoverable || d.Action != ActionUnrecoverable {
			t.Fatalf("case %d = %+v", i, d)
		}
	}
}

func TestDecideDeterministic(t *testing.T) {
	e := Evidence{HoleStartUS: 100, PreviousEndUS: 100, NextStartUS: 900, HasNext: true}
	if Decide(e) != Decide(e) {
		t.Fatal("decision must be pure")
	}
}

func TestStatsRate(t *testing.T) {
	if (Stats{}).Rate() != 1 {
		t.Fatal("no-loss run must be 1.0")
	}
	s := Stats{LossHoles: 10, RecoveredLoss: 9}
	if s.Rate() != 0.9 {
		t.Fatalf("rate = %v", s.Rate())
	}
}
