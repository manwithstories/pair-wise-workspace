package fault

import "testing"

func TestBlockingMatrix(t *testing.T) {
	cases := []struct {
		sev      Severity
		policy   Policy
		blocking bool
	}{
		{Recoverable, PolicyStrict, false},
		{Recoverable, PolicySkip, false},
		{Skip, PolicyStrict, true},
		{Skip, PolicySkip, false},
		{Fatal, PolicyStrict, true},
		{Fatal, PolicySkip, true},
	}
	for _, c := range cases {
		l := &List{}
		l.Add(c.sev, 10, 1, 20, "x")
		if got := l.Blocking(c.policy); got != c.blocking {
			t.Errorf("sev=%v policy=%v: blocking=%v want %v", c.sev, c.policy, got, c.blocking)
		}
	}
}

func TestEmptyListNotBlocking(t *testing.T) {
	l := &List{}
	if l.Blocking(PolicyStrict) {
		t.Fatal("empty list must not block")
	}
	if l.MaxSeverity() != Recoverable {
		t.Fatal("empty list severity should be Recoverable")
	}
}

func TestCounts(t *testing.T) {
	l := &List{}
	l.Add(Recoverable, -1, 0, -1, "a")
	l.Add(Skip, -1, 0, -1, "b")
	l.Add(Skip, -1, 0, -1, "c")
	counts := l.CountBySeverity()
	if counts[Skip] != 2 || counts[Recoverable] != 1 {
		t.Fatalf("counts: %v", counts)
	}
}
