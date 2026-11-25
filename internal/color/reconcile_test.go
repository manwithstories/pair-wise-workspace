package color

import (
	"strings"
	"testing"

	"fieldqc/internal/clock"
	"fieldqc/internal/fault"
)

func TestReconcileUnanimous(t *testing.T) {
	metas := []Meta{
		{Source: "t1", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"},
		{Source: "t2", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"},
	}
	got, conflicts := Reconcile(metas)
	if conflicts != nil {
		t.Fatalf("unexpected conflicts: %+v", conflicts)
	}
	if got.Source != "t1" || got.Primaries != "bt709" {
		t.Fatalf("unanimous meta = %+v", got)
	}
}

func TestReconcileEmpty(t *testing.T) {
	if got, c := Reconcile(nil); c != nil || got != (Meta{}) {
		t.Fatalf("empty Reconcile = %+v %+v", got, c)
	}
}

func TestConflictPreservesTrackSourcesAndDoesNotOverride(t *testing.T) {
	metas := []Meta{
		{Source: "card0/track1", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"},
		{Source: "card0/track2", Primaries: "bt2020", Transfer: "bt2020", Matrix: "bt2020nc"},
	}
	unanimous, conflicts := Reconcile(metas)
	if unanimous != (Meta{}) {
		t.Fatalf("conflict must not silently pick a winner, got %+v", unanimous)
	}
	if len(conflicts) != 3 { // primaries, transfer, matrix (range agrees limited)
		t.Fatalf("conflicts=%d, want 3: %+v", len(conflicts), conflicts)
	}
	byField := map[string]Conflict{}
	for _, c := range conflicts {
		byField[c.Field] = c
	}
	pc, ok := byField["primaries"]
	if !ok {
		t.Fatal("missing primaries conflict")
	}
	if pc.Values["card0/track1"] != "bt709" || pc.Values["card0/track2"] != "bt2020" {
		t.Fatalf("per-track sources not preserved: %+v", pc.Values)
	}
}

func TestSpansAreDeterministicAndSourced(t *testing.T) {
	metas := []Meta{
		{Source: "a", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"},
		{Source: "b", Primaries: "bt2020", Transfer: "bt709", Matrix: "bt709"},
	}
	_, conflicts := Reconcile(metas)
	start, end := clock.Zero(), clock.Ratio{Num: 10, Den: 1}
	spans := Spans(conflicts, start, end)
	if len(spans) != 1 {
		t.Fatalf("spans=%d, want 1 (only primaries differs)", len(spans))
	}
	sp := spans[0]
	if sp.Track != "color" || sp.Code != fault.CodeColorConflict {
		t.Fatalf("span identity wrong: %+v", sp)
	}
	if sp.Start.Cmp(start) != 0 || sp.End.Cmp(end) != 0 {
		t.Fatalf("span range = %s..%s, want 0..10", sp.Start, sp.End)
	}
	if !strings.Contains(sp.Detail, "a=bt709") || !strings.Contains(sp.Detail, "b=bt2020") {
		t.Fatalf("detail loses track sources: %q", sp.Detail)
	}
}
