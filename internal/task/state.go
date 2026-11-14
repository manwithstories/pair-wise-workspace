// Package task contains business-agnostic segment state, journaling and
// concurrent scheduling primitives.
package task

import "fmt"

type State string

const (
	Pending   State = "PENDING"
	Probing   State = "PROBING"
	Planning  State = "PLANNING"
	Encoding  State = "ENCODING"
	Verifying State = "VERIFYING"
	Done      State = "DONE"
	Retrying  State = "RETRYING"
	Failed    State = "FAILED"
	Aborted   State = "ABORTED"
)

var ErrIllegalTransition = fmt.Errorf("illegal task state transition")

var transitions = map[State]map[State]bool{
	Pending:   {Probing: true, Planning: true, Aborted: true},
	Probing:   {Planning: true, Retrying: true, Failed: true, Aborted: true, Pending: true},
	Planning:  {Encoding: true, Failed: true, Aborted: true, Pending: true},
	Encoding:  {Verifying: true, Retrying: true, Failed: true, Aborted: true, Pending: true},
	Verifying: {Done: true, Retrying: true, Failed: true, Aborted: true, Pending: true},
	Retrying:  {Probing: true, Encoding: true, Failed: true, Aborted: true, Pending: true},
	Failed:    {Pending: true, Aborted: true},
	Aborted:   {Pending: true},
	Done:      {},
}

func ValidState(s State) bool {
	_, ok := transitions[s]
	return ok
}

func CanTransition(from, to State) bool {
	return transitions[from][to]
}

func CheckTransition(from, to State) error {
	if !ValidState(from) || !ValidState(to) {
		return fmt.Errorf("%w: unknown state %q -> %q", ErrIllegalTransition, from, to)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, from, to)
	}
	return nil
}
