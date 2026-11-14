package rtsp

import "testing"

func TestStateMachineHappyPath(t *testing.T) {
	m := NewStateMachine()
	for _, st := range OrderedStates[1:] {
		if err := m.Move(st); err != nil {
			t.Fatalf("move to %s: %v", st, err)
		}
	}
	if got := m.LegalTransitionCount(); got != 6 {
		t.Fatalf("transitions = %d, want 6", got)
	}
	if m.Current() != StateTeardown || m.Failures() != 0 {
		t.Fatalf("unexpected final state %q failures %d", m.Current(), m.Failures())
	}
	if len(m.Transitions()) != 6 {
		t.Fatalf("log len = %d, want 6", len(m.Transitions()))
	}
}

func TestStateMachineIllegalMove(t *testing.T) {
	m := NewStateMachine()
	if err := m.Move(StatePlay); err == nil {
		t.Fatal("expected illegal Init->Play to fail")
	}
	if m.Failures() != 1 || m.Current() != StateInit {
		t.Fatalf("failures=%d state=%q", m.Failures(), m.Current())
	}
	if err := m.Move(StateOptions); err != nil {
		t.Fatal(err)
	}
	if m.LegalTransitionCount() != 1 {
		t.Fatalf("legal count = %d, want 1", m.LegalTransitionCount())
	}
	tr := m.Transitions()
	if tr[0].Legal || tr[0].Reason == "" {
		t.Fatalf("illegal transition not recorded: %+v", tr[0])
	}
}

func TestStateMachineReconnectEdges(t *testing.T) {
	m := NewStateMachine()
	_ = m.Move(StateOptions)
	_ = m.Move(StateDescribe)
	_ = m.Move(StateSetup)
	_ = m.Move(StatePlay)
	_ = m.Move(StatePause)
	if err := m.Move(StatePlay); err != nil {
		t.Fatalf("Pause->Play resume edge: %v", err)
	}
	_ = m.Move(StatePause)
	if err := m.Move(StateTeardown); err != nil {
		t.Fatal(err)
	}
}
