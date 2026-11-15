package rtsp

import "sync"

// State is one explicit RTSP session state.
type State string

const (
	StateInitial  State = "initial"
	StateReady    State = "ready"
	StatePlaying  State = "playing"
	StatePaused   State = "paused"
	StateTeardown State = "teardown"
	StateFailed   State = "failed"
)

// Event is input to the session transition table.
type Event string

const (
	EventOPTIONS             Event = "OPTIONS"
	EventDESCRIBE            Event = "DESCRIBE"
	EventSETUP               Event = "SETUP"
	EventPLAY                Event = "PLAY"
	EventPAUSE               Event = "PAUSE"
	EventTEARDOWN            Event = "TEARDOWN"
	EventSessionTimeout      Event = "SESSION_TIMEOUT"
	EventTransportDisconnect Event = "TRANSPORT_DISCONNECT"
	EventUDPSilence          Event = "UDP_SILENCE"
)

type transitionKey struct {
	from  State
	event Event
}

var transitionTable = map[transitionKey]State{
	{StateInitial, EventOPTIONS}:             StateReady,
	{StateReady, EventOPTIONS}:               StateReady,
	{StateReady, EventDESCRIBE}:              StateReady,
	{StateReady, EventSETUP}:                 StateReady,
	{StateReady, EventPLAY}:                  StatePlaying,
	{StateReady, EventTEARDOWN}:              StateTeardown,
	{StateReady, EventSessionTimeout}:        StateFailed,
	{StateReady, EventTransportDisconnect}:   StateFailed,
	{StatePlaying, EventPLAY}:                StatePlaying,
	{StatePlaying, EventPAUSE}:               StatePaused,
	{StatePlaying, EventTEARDOWN}:            StateTeardown,
	{StatePlaying, EventSessionTimeout}:      StateFailed,
	{StatePlaying, EventTransportDisconnect}: StateFailed,
	{StatePlaying, EventUDPSilence}:          StatePlaying,
	{StatePaused, EventPLAY}:                 StatePlaying,
	{StatePaused, EventTEARDOWN}:             StateTeardown,
	{StatePaused, EventSessionTimeout}:       StateFailed,
	{StatePaused, EventTransportDisconnect}:  StateFailed,
	{StatePaused, EventUDPSilence}:           StateFailed,
}

// StateTransition records one applied event.
type StateTransition struct {
	From  State `json:"from"`
	Event Event `json:"event"`
	To    State `json:"to"`
}

// TransitionResult describes one Apply call.
type TransitionResult struct {
	Transition StateTransition
	Applied    bool
	Illegal    bool
}

// Machine applies the explicit table under a mutex.
type Machine struct {
	mu          sync.Mutex
	state       State
	transitions []StateTransition
	illegal     int
}

// NewMachine starts in the initial state.
func NewMachine() *Machine {
	return &Machine{state: StateInitial}
}

// Apply either changes state or records an illegal transition without mutation.
func (m *Machine) Apply(event Event) TransitionResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	to, ok := transitionTable[transitionKey{from: m.state, event: event}]
	if !ok {
		m.illegal++
		return TransitionResult{
			Transition: StateTransition{From: m.state, Event: event, To: m.state},
			Illegal:    true,
		}
	}
	record := StateTransition{From: m.state, Event: event, To: to}
	m.state = to
	m.transitions = append(m.transitions, record)
	return TransitionResult{Transition: record, Applied: true}
}

// State returns the current state.
func (m *Machine) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// TransitionCount returns the number of applied transitions.
func (m *Machine) TransitionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.transitions)
}

// IllegalTransitions returns the number of rejected events.
func (m *Machine) IllegalTransitions() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.illegal
}

// History returns a copy of applied transitions.
func (m *Machine) History() []StateTransition {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]StateTransition(nil), m.transitions...)
}
