package graph

import (
	"reflect"
	"testing"
)

func TestOrder(t *testing.T) {
	ids := []string{"c", "a", "b"}
	deps := map[string][]string{"b": {"a"}, "c": {"b"}}
	got, err := Order(ids, deps)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestOrderUnknownDep(t *testing.T) {
	_, err := Order([]string{"a"}, map[string][]string{"a": {"ghost"}})
	if err == nil {
		t.Fatal("want error")
	}
}

func TestOrderCycle(t *testing.T) {
	_, err := Order([]string{"a", "b"}, map[string][]string{"a": {"b"}, "b": {"a"}})
	if err == nil {
		t.Fatal("want cycle error")
	}
}
