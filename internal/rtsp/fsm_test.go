package rtsp

import "testing"

func TestMachinePullSequence(t *testing.T) {
	m := NewMachine()
	for _, event := range []Event{EventOPTIONS, EventDESCRIBE, EventSETUP, EventPLAY, EventTEARDOWN} {
		if result := m.Apply(event); !result.Applied || result.Illegal {
			t.Fatalf("event %s rejected: %+v", event, result)
		}
	}
	if got := m.State(); got != StateTeardown {
		t.Fatalf("state=%s", got)
	}
	if m.TransitionCount() != 5 || m.IllegalTransitions() != 0 {
		t.Fatalf("transitions=%d illegal=%d", m.TransitionCount(), m.IllegalTransitions())
	}
}

func TestMachineIllegalDoesNotMutate(t *testing.T) {
	m := NewMachine()
	result := m.Apply(EventPLAY)
	if !result.Illegal || result.Applied {
		t.Fatalf("result=%+v", result)
	}
	if m.State() != StateInitial || m.TransitionCount() != 0 || m.IllegalTransitions() != 1 {
		t.Fatalf("state=%s transitions=%d illegal=%d", m.State(), m.TransitionCount(), m.IllegalTransitions())
	}
}

func TestMachineUDPRecoveryAndFailure(t *testing.T) {
	m := NewMachine()
	for _, event := range []Event{EventOPTIONS, EventDESCRIBE, EventSETUP, EventPLAY} {
		m.Apply(event)
	}
	if result := m.Apply(EventUDPSilence); !result.Applied || m.State() != StatePlaying {
		t.Fatalf("silence recovery failed: %+v", result)
	}
	if result := m.Apply(EventTransportDisconnect); !result.Applied || m.State() != StateFailed {
		t.Fatalf("disconnect failed: %+v", result)
	}
}
