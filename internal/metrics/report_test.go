package metrics

import (
	"strings"
	"testing"
)

func TestRenderSortedKeyValue(t *testing.T) {
	r := New()
	r.Set("zebra", "1")
	r.Set("alpha", "2")
	out := r.Render()
	ai := strings.Index(out, "alpha=2")
	zi := strings.Index(out, "zebra=1")
	if ai < 0 || zi < 0 || ai > zi {
		t.Fatalf("output not sorted key=value:\n%s", out)
	}
}

func TestChecksGate(t *testing.T) {
	r := New()
	if !r.ChecksPass() {
		t.Fatal("empty checks should pass")
	}
	r.Check("a", true, "ok")
	r.Check("b", false, "bad")
	if r.ChecksPass() {
		t.Fatal("failing check not detected")
	}
	if !strings.Contains(r.Render(), "check_b=FAIL") {
		t.Fatal("failing check not rendered")
	}
}

func TestFailuresRecorded(t *testing.T) {
	r := New()
	r.AddFailure("parse", "box overflow at 100")
	if len(r.Failures()) != 1 {
		t.Fatal("failure not recorded")
	}
	if !strings.Contains(r.Render(), "failure_1=parse: box overflow at 100") {
		t.Fatalf("failure not rendered:\n%s", r.Render())
	}
}
