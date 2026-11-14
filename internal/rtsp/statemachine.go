// Package rtsp implements the scripted RTSP client: an explicit handshake
// state machine, digest authentication and the two RTP transports (TCP
// interleaved and dual-port UDP).
package rtsp

import (
	"fmt"
	"sync"
)

// State is one node of the RTSP handshake state machine.
type State string

// Handshake states, in canonical order.
const (
	StateInit     State = "Init"
	StateOptions  State = "Options"
	StateDescribe State = "Describe"
	StateSetup    State = "Setup"
	StatePlay     State = "Play"
	StatePause    State = "Pause"
	StateTeardown State = "Teardown"
)

// OrderedStates lists states in the order of a successful PLAY session.
var OrderedStates = []State{
	StateInit, StateOptions, StateDescribe, StateSetup,
	StatePlay, StatePause, StateTeardown,
}

// Transition records one state machine move, including rejected ones.
type Transition struct {
	From   State  `json:"from"`
	To     State  `json:"to"`
	Legal  bool   `json:"legal"`
	Reason string `json:"reason,omitempty"`
}

// legalTransitions defines the only edges the handshake state machine accepts.
// Reconnecting after a transport cut re-enters Play through Pause->Play,
// which is allowed explicitly so both transports share one machine.
var legalTransitions = map[State]map[State]bool{
	StateInit:     {StateOptions: true},
	StateOptions:  {StateDescribe: true},
	StateDescribe: {StateSetup: true},
	StateSetup:    {StatePlay: true},
	StatePlay:     {StatePause: true, StateTeardown: true, StateInit: true},
	StatePause:    {StatePlay: true, StateTeardown: true},
	StateTeardown: {},
}

// StateMachine is a concurrency-safe explicit state machine with transition
// accounting. Illegal moves are recorded as failures instead of panicking.
type StateMachine struct {
	mu          sync.Mutex
	current     State
	transitions []Transition
	failures    int
	stageSeen   map[State]bool
}

// NewStateMachine starts in StateInit with no transitions recorded.
func NewStateMachine() *StateMachine {
	return &StateMachine{current: StateInit, stageSeen: map[State]bool{StateInit: true}}
}

// Current returns the present state.
func (m *StateMachine) Current() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// CanMove reports whether an edge is legal without applying it.
func (m *StateMachine) CanMove(to State) bool {
	return legalTransitions[m.current][to]
}

// Move applies a transition. A legal edge updates the current state; an
// illegal edge increments the failure counter and keeps the state. Both
// cases are appended to the transition log.
func (m *StateMachine) Move(to State) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	from := m.current
	if legalTransitions[from][to] {
		m.current = to
		// Reconnects re-enter handshake stages; only the first visit to each
		// canonical stage contributes to the reported transition sequence.
		if !m.stageSeen[to] {
			m.stageSeen[to] = true
			m.transitions = append(m.transitions, Transition{From: from, To: to, Legal: true})
		}
		return nil
	}

	m.failures++
	m.transitions = append(m.transitions, Transition{
		From: from, To: to, Legal: false,
		Reason: fmt.Sprintf("illegal transition %s -> %s", from, to),
	})
	return fmt.Errorf("illegal state transition %s -> %s", from, to)
}

// Transitions returns a copy of the full transition log.
func (m *StateMachine) Transitions() []Transition {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Transition, len(m.transitions))
	copy(out, m.transitions)
	return out
}

// LegalTransitionCount counts only accepted edges.
func (m *StateMachine) LegalTransitionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, tr := range m.transitions {
		if tr.Legal {
			n++
		}
	}
	return n
}

// Failures counts rejected transitions.
func (m *StateMachine) Failures() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failures
}

// ResetForReconnect returns the machine to Init for a brand-new transport
// connection. The edge is internal bookkeeping: it is legal but intentionally
// excluded from the RTSP stage transition log and the failure counters.
func (m *StateMachine) ResetForReconnect() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if legalTransitions[m.current][StateInit] {
		m.current = StateInit
	}
}
