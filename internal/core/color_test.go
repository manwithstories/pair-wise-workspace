package core

import (
	"testing"

	"mediacheck/internal/model"
)

func cm(p, tr, m uint16) *model.ColorMeta {
	return &model.ColorMeta{ColorPrimaries: p, Transfer: tr, Matrix: m}
}

func TestResolveColorNoConflict(t *testing.T) {
	src := []*model.ColorMeta{cm(1, 1, 1), cm(1, 1, 1)}
	dec := ResolveColor(src, ColorPriority{Default: model.ColorMeta{ColorPrimaries: 1}})
	if dec.Conflicts != 0 || dec.Selected.ColorPrimaries != 1 {
		t.Fatalf("decision = %+v", dec)
	}
}

func TestResolveColorConflictByPriority(t *testing.T) {
	src := []*model.ColorMeta{
		cm(1, 1, 1),  // BT.709
		cm(9, 16, 9), // BT.2020/HLG
		cm(9, 16, 9),
	}
	prio := ColorPriority{
		Order:   []string{"9/16/9", "1/1/1"},
		Default: model.ColorMeta{ColorPrimaries: 1},
	}
	dec := ResolveColor(src, prio)
	if dec.Conflicts != 1 {
		t.Fatalf("conflicts = %d, want 1 (two distinct values)", dec.Conflicts)
	}
	if dec.Selected.ColorPrimaries != 9 || dec.Selected.Transfer != 16 {
		t.Fatalf("priority selection wrong: %+v", dec.Selected)
	}
	if len(dec.Sources) != 3 {
		t.Fatalf("source values retained = %d", len(dec.Sources))
	}
}

func TestResolveColorFallsBackWhenUnlisted(t *testing.T) {
	src := []*model.ColorMeta{cm(5, 5, 5), cm(6, 6, 6)}
	prio := ColorPriority{
		Order:   []string{"1/1/1"},
		Default: model.ColorMeta{ColorPrimaries: 1, Transfer: 1, Matrix: 1},
	}
	dec := ResolveColor(src, prio)
	if dec.Conflicts != 1 {
		t.Fatalf("conflicts = %d", dec.Conflicts)
	}
	if dec.Selected != prio.Default {
		t.Fatalf("must fall back to default, got %+v", dec.Selected)
	}
}

func TestResolveColorEmptyUsesDefault(t *testing.T) {
	def := model.ColorMeta{ColorPrimaries: 1}
	dec := ResolveColor(nil, ColorPriority{Default: def})
	if dec.Selected != def || dec.Conflicts != 0 {
		t.Fatalf("empty sources decision = %+v", dec)
	}
}
