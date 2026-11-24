package adapter

import (
	"testing"

	"media-check/internal/model"
)

type dummy struct{ id, ver string }

func (d dummy) ID() string      { return d.id }
func (d dummy) Version() string { return d.ver }

func TestRegistryResolveAndReject(t *testing.T) {
	r := NewRegistry()
	r.Register(dummy{"fmt-a", "v1"})
	r.Register(dummy{"fmt-a", "v2"})

	a, fault := r.Get("fmt-a", "v2")
	if fault != nil || a.(dummy).ver != "v2" {
		t.Fatal("versioned resolve failed")
	}
	_, fault = r.Get("fmt-a", "v9")
	if fault == nil || fault.Code != model.FaultUnknownAdapter {
		t.Fatal("unknown version must be rejected")
	}
	if fault.Adapter != "fmt-a@v9" {
		t.Fatalf("fault must name the adapter identity, got %q", fault.Adapter)
	}
	if got := r.List(); len(got) != 2 || got[0] != "fmt-a@v1" {
		t.Fatalf("list not deterministic: %v", got)
	}
}

func TestRegistryDuplicatePanics(t *testing.T) {
	r := NewRegistry()
	r.Register(dummy{"x", "v1"})
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration must panic")
		}
	}()
	r.Register(dummy{"x", "v1"})
}
