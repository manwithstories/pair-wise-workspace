package timeline

import (
	"reflect"
	"testing"
)

func TestDefaultGraphOrder(t *testing.T) {
	got, err := ValidateDefaultGraph()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"init", "sample-index", "event-map", "remux", "manifest"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestGraphMissingDependency(t *testing.T) {
	_, err := TopoSort([]Node{{Name: "a", Deps: []string{"missing"}}})
	if !IsCode(err, CodeMissingDependency) {
		t.Fatalf("expected missing dependency, got %v", err)
	}
}

func TestGraphCycle(t *testing.T) {
	nodes := []Node{
		{Name: "a", Deps: []string{"b"}},
		{Name: "b", Deps: []string{"c"}},
		{Name: "c", Deps: []string{"a"}},
	}
	_, err := TopoSort(nodes)
	if !IsCode(err, CodeCycle) {
		t.Fatalf("expected cycle error, got %v", err)
	}
}
