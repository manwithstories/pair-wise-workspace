// Package state provides the persisted job state machine and crash-safe
// checkpoint journal.
package state

import (
	"errors"
	"fmt"
	"sync"
)

type State string

const (
	Pending    State = "pending"
	Validating State = "validating"
	Analyzing  State = "analyzing"
	Encoding   State = "encoding"
	Verifying  State = "verifying"
	Done       State = "done"
	Paused     State = "paused"
	Failed     State = "failed"
	Cancelled  State = "cancelled"
)

var ErrInvalidTransition = errors.New("invalid state transition")

var transitionTable = map[State][]State{
	Pending:    {Validating, Cancelled, Failed},
	Validating: {Analyzing, Failed, Cancelled},
	Analyzing:  {Encoding, Paused, Failed, Cancelled},
	Encoding:   {Verifying, Paused, Failed, Cancelled},
	Verifying:  {Done, Failed, Cancelled},
	Paused:     {Encoding, Failed, Cancelled},
	Failed:     {Pending, Cancelled},
	Cancelled:  {},
	Done:       {},
}

// Machine is safe to use from scheduler goroutines.
type Machine struct {
	mu    sync.RWMutex
	state State
}

func New() *Machine { return &Machine{state: Pending} }

func (m *Machine) State() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// Transition atomically applies next, or leaves the previous state untouched.
func (m *Machine) Transition(next State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !CanTransition(m.state, next) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, m.state, next)
	}
	m.state = next
	return nil
}

func (m *Machine) Force(s State) {
	m.mu.Lock()
	m.state = s
	m.mu.Unlock()
}

func CanTransition(from, to State) bool {
	for _, candidate := range transitionTable[from] {
		if candidate == to {
			return true
		}
	}
	return false
}

// Transitions returns a defensive copy of the migration table.
func Transitions() map[State][]State {
	out := make(map[State][]State, len(transitionTable))
	for from, tos := range transitionTable {
		out[from] = append([]State(nil), tos...)
	}
	return out
}
