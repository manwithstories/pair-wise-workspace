package normalize

import "testing"

func TestColorRegistryNoConflict(t *testing.T) {
	r := NewColorRegistry()
	r.Record(ColorObservation{Source: "cam1/pri/0", Primaries: "bt709", Transfer: "bt709", Space: "bt709", Range: "tv"})
	r.Record(ColorObservation{Source: "cam1/pri/1", Primaries: "bt709", Transfer: "bt709", Space: "bt709", Range: "tv"})
	if c := r.Analyse(); len(c) != 0 {
		t.Fatalf("expected no conflict, got %+v", c)
	}
}

func TestColorRegistryDetectsConflictAndProvenance(t *testing.T) {
	r := NewColorRegistry()
	r.Record(ColorObservation{Source: "cam1/pri", Primaries: "bt709", Transfer: "bt709", Space: "bt709", Range: "tv"})
	r.Record(ColorObservation{Source: "cam1/bak", Primaries: "bt2020", Transfer: "bt709", Space: "bt709", Range: "tv"})
	conflicts := r.Analyse()
	if len(conflicts) == 0 {
		t.Fatal("expected conflicts")
	}
	var primary *ColorConflict
	for i := range conflicts {
		if conflicts[i].Field == "primaries" {
			primary = &conflicts[i]
		}
	}
	if primary == nil {
		t.Fatalf("missing primaries conflict: %+v", conflicts)
	}
	if primary.Dominant != "bt709" {
		t.Fatalf("dominant = %q, want bt709", primary.Dominant)
	}
	// A 1-vs-1 split has no strict majority -> ambiguous.
	if !primary.Ambiguous {
		t.Fatal("tie split must be ambiguous")
	}
	if len(primary.Sources) != 2 {
		t.Fatalf("sources = %d, want 2 (provenance)", len(primary.Sources))
	}
}

func TestColorRegistryDominantNotAmbiguous(t *testing.T) {
	r := NewColorRegistry()
	for i := 0; i < 3; i++ {
		r.Record(ColorObservation{Source: "pri", Primaries: "bt709", Transfer: "bt709", Space: "bt709", Range: "tv"})
	}
	r.Record(ColorObservation{Source: "bak", Primaries: "bt2020", Transfer: "bt709", Space: "bt709", Range: "tv"})
	var primary *ColorConflict
	cs := r.Analyse()
	for i := range cs {
		if cs[i].Field == "primaries" {
			primary = &cs[i]
		}
	}
	if primary == nil {
		t.Fatal("expected primaries conflict")
	}
	if primary.Ambiguous {
		t.Fatal("3-vs-1 should not be ambiguous")
	}
	if primary.Dominant != "bt709" {
		t.Fatalf("dominant = %q", primary.Dominant)
	}
}

func TestColorRegistryIgnoresUnspecified(t *testing.T) {
	r := NewColorRegistry()
	r.Record(ColorObservation{Source: "a", Primaries: "unspecified", Transfer: "unspecified", Space: "unspecified", Range: "unspecified"})
	r.Record(ColorObservation{Source: "b", Primaries: "bt709", Transfer: "bt709", Space: "bt709", Range: "tv"})
	if c := r.Analyse(); len(c) != 0 {
		t.Fatalf("unspecified must not trigger conflict, got %+v", c)
	}
}
