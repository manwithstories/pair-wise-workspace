// Package state provides segment state transitions and a durable append journal.
package state

import "fmt"

type Status string

const (
	Pending   Status = "pending"
	Planned   Status = "planned"
	Encoding  Status = "encoding"
	Verifying Status = "verifying"
	Done      Status = "done"
	Failed    Status = "failed"
)

var legal = map[Status]map[Status]bool{
	Pending:   {Planned: true},
	Planned:   {Encoding: true, Failed: true},
	Encoding:  {Verifying: true, Failed: true},
	Verifying: {Done: true, Failed: true},
	Failed:    {Encoding: true},
	Done:      {},
}

func (s Status) Valid() bool {
	_, ok := legal[s]
	return ok
}

func CanTransition(from, to Status) bool {
	return legal[from][to]
}

type IllegalTransitionError struct {
	From Status
	To   Status
}

func (e *IllegalTransitionError) Error() string {
	return fmt.Sprintf("illegal state transition %s -> %s", e.From, e.To)
}

// Machine enforces the segment lifecycle. It is intentionally not concurrent.
type Machine struct {
	status Status
}

func NewMachine(initial Status) (*Machine, error) {
	if !initial.Valid() {
		return nil, fmt.Errorf("unknown state %q", initial)
	}
	return &Machine{status: initial}, nil
}

func (m *Machine) Status() Status { return m.status }

func (m *Machine) Transition(next Status) error {
	if !next.Valid() {
		return fmt.Errorf("unknown state %q", next)
	}
	if !CanTransition(m.status, next) {
		return &IllegalTransitionError{From: m.status, To: next}
	}
	m.status = next
	return nil
}

// MustTransition is useful inside trusted stage sequencing.
func (m *Machine) MustTransition(next Status) {
	if err := m.Transition(next); err != nil {
		panic(err)
	}
}
