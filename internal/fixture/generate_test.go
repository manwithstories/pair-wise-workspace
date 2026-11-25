package fixture

import (
	"testing"

	"mediqc/internal/audit"
)

func TestSuiteIsDeterministicAndComplete(t *testing.T) {
	a, err := Suite(412)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Suite(412)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 6 {
		t.Fatalf("cases=%d want 6", len(a))
	}
	counts := map[string]int{}
	for i := range a {
		counts[a[i].ExpectedClass]++
		if audit.HashBytes(a[i].TS) != audit.HashBytes(b[i].TS) {
			t.Fatalf("case %s is not deterministic", a[i].ID)
		}
		if a[i].TraceID == "" || len(a[i].Tracks) != 2 {
			t.Fatalf("case %s missing trace/tracks", a[i].ID)
		}
	}
	if counts[ClassClean] != 1 || counts[ClassRepaired] != 4 || counts[ClassRejected] != 1 {
		t.Fatalf("class counts=%v", counts)
	}
}
