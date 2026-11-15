package state

import (
	"errors"
	"testing"
)

func TestLegalLifecycle(t *testing.T) {
	m, err := NewMachine(Pending)
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []Status{Planned, Encoding, Verifying, Done} {
		if err := m.Transition(next); err != nil {
			t.Fatalf("%s -> %s: %v", m.Status(), next, err)
		}
	}
}

func TestIllegalTransition(t *testing.T) {
	m, _ := NewMachine(Pending)
	err := m.Transition(Done)
	var target *IllegalTransitionError
	if !errors.As(err, &target) {
		t.Fatalf("expected illegal transition, got %v", err)
	}
}

func TestFailedCanBeRetried(t *testing.T) {
	m, _ := NewMachine(Planned)
	if err := m.Transition(Encoding); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(Failed); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(Encoding); err != nil {
		t.Fatal(err)
	}
}
