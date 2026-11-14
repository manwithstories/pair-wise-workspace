// Package job manages encode job state, checkpoints and scheduling.
package job

import (
	"fmt"
	"sort"
	"strings"
)

// State is the job lifecycle state.
type State string

const (
	StatePending     State = "pending"
	StateRunning     State = "running"
	StateInterrupted State = "interrupted"
	StateCompleted   State = "completed"
	StateFailed      State = "failed"
)

// transitions lists legal successors for each state.
var transitions = map[State][]State{
	StatePending:     {StateRunning},
	StateRunning:     {StateInterrupted, StateCompleted, StateFailed},
	StateInterrupted: {StateRunning},
	StateFailed:      {StateRunning}, // resume may rehabilitate a failed job
	StateCompleted:   {},
}

// Successors returns the legal next states, sorted for stable output.
func (s State) Successors() []State {
	out := append([]State(nil), transitions[s]...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// CanTransition reports whether moving from s to next is legal.
func (s State) CanTransition(next State) bool {
	for _, t := range transitions[s] {
		if t == next {
			return true
		}
	}
	return false
}

// Machine is a job state machine that rejects illegal transitions.
type Machine struct {
	state State
}

// NewMachine creates a machine in the given state.
func NewMachine(initial State) *Machine { return &Machine{state: initial} }

// State returns the current state.
func (m *Machine) State() State { return m.state }

// Transition moves to next or returns an error naming the current
// state and its legal successors.
func (m *Machine) Transition(next State) error {
	if !m.state.CanTransition(next) {
		succ := m.state.Successors()
		readable := make([]string, len(succ))
		for i, s := range succ {
			readable[i] = string(s)
		}
		if len(readable) == 0 {
			readable = []string{"(none, terminal)"}
		}
		return fmt.Errorf("illegal transition %s -> %s: current state %q allows [%s]",
			m.state, next, m.state, strings.Join(readable, ", "))
	}
	m.state = next
	return nil
}
