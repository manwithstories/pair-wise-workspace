// Package session implements the RTSP control-plane state machine shared by
// all transports. Media-plane reassembly lives elsewhere; this FSM only
// tracks where the control conversation is, rejects illegal transitions and
// guards against states that run past their deadline.
package session

import (
	"fmt"
	"time"
)

// State is a control-plane state of an RTSP session.
type State int

const (
	StateInit State = iota
	StateOptions
	StateDescribe
	StateSetup
	StatePlaying
	StatePaused
	StateTeardown
)

func (s State) String() string {
	switch s {
	case StateInit:
		return "INIT"
	case StateOptions:
		return "OPTIONS"
	case StateDescribe:
		return "DESCRIBE"
	case StateSetup:
		return "SETUP"
	case StatePlaying:
		return "PLAYING"
	case StatePaused:
		return "PAUSED"
	case StateTeardown:
		return "TEARDOWN"
	}
	return "UNKNOWN"
}

// legalTransitions lists every edge the control plane may take. DESCRIBE has
// a self-edge so a 401 digest-auth retry can re-enter the state. Every
// non-terminal state may fall into TEARDOWN on error or timeout.
var legalTransitions = map[State][]State{
	StateInit:     {StateOptions, StateTeardown},
	StateOptions:  {StateDescribe, StateTeardown},
	StateDescribe: {StateDescribe, StateSetup, StateTeardown},
	StateSetup:    {StatePlaying, StateTeardown},
	StatePlaying:  {StatePaused, StateTeardown},
	StatePaused:   {StatePlaying, StateTeardown},
	StateTeardown: {},
}

// FSM is a single session state machine instance. Both transports drive the
// same FSM; only the transport implementation is swapped underneath.
type FSM struct {
	state        State
	count        int
	enteredAt    time.Time
	stateTimeout time.Duration
	now          func() time.Time
}

// New returns an FSM in INIT. stateTimeout is the per-state dwell guard: any
// state that is not left within the budget is considered stuck and Expired
// reports true, after which the caller must move to TEARDOWN.
func New(stateTimeout time.Duration) *FSM {
	return &FSM{
		state:        StateInit,
		enteredAt:    time.Now(),
		stateTimeout: stateTimeout,
		now:          time.Now,
	}
}

// State returns the current state.
func (f *FSM) State() State { return f.state }

// Transitions returns how many legal transitions have been taken so far.
func (f *FSM) Transitions() int { return f.count }

// Can reports whether moving from the current state to to is legal.
func (f *FSM) Can(to State) bool {
	for _, s := range legalTransitions[f.state] {
		if s == to {
			return true
		}
	}
	return false
}

// Transition moves the FSM to to. Illegal moves are rejected, leave the
// state untouched and are not counted.
func (f *FSM) Transition(to State) error {
	if !f.Can(to) {
		return fmt.Errorf("session: illegal transition %s -> %s", f.state, to)
	}
	f.state = to
	f.count++
	f.enteredAt = f.now()
	return nil
}

// Teardown forces the FSM into TEARDOWN from any non-terminal state. It is
// the single funnel for errors, server disconnects and timeout guards, and
// is a no-op once TEARDOWN has been reached.
func (f *FSM) Teardown() {
	if f.state == StateTeardown {
		return
	}
	f.state = StateTeardown
	f.count++
	f.enteredAt = f.now()
}

// Expired reports whether the current (non-terminal) state has dwelled
// longer than the configured guard.
func (f *FSM) Expired() bool {
	if f.state == StateTeardown {
		return false
	}
	return f.now().Sub(f.enteredAt) > f.stateTimeout
}
