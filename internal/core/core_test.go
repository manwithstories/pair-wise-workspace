package core

import "testing"

func TestParseSecondsIntegerTicks(t *testing.T) {
	cases := map[string]Ticks{
		"6":       6_000_000_000,
		"6.000":   6_000_000_000,
		"2.500":   2_500_000_000,
		"0.00025": 250_000,
	}
	for in, want := range cases {
		got, err := ParseSeconds(in)
		if err != nil {
			t.Fatalf("ParseSeconds(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseSeconds(%q)=%d want %d", in, got, want)
		}
	}
	if _, err := ParseSeconds("-1.5"); err == nil {
		t.Fatal("negative duration accepted")
	}
}

func TestTimelineRejectsRollbackButKeepsNonZeroBase(t *testing.T) {
	tl := Timeline{Scale: 90000}
	for _, v := range []Ticks{900000, 903600, 907200} {
		if err := tl.Observe(v); err != nil {
			t.Fatal(err)
		}
	}
	if tl.Base != 900000 {
		t.Fatalf("non-zero base normalized: %d", tl.Base)
	}
	if err := tl.Observe(900001); err == nil {
		t.Fatal("timestamp rollback accepted")
	}
}

func TestExitCodeClassification(t *testing.T) {
	if got := ExitCode(ManifestError("x", 3, "bad")); got != ExitUsage {
		t.Fatalf("manifest exit=%d", got)
	}
	if got := ExitCode(UnrecoverableError(StageTransport, "x", "seg", 10, "gap")); got != ExitUnrecoverable {
		t.Fatalf("transport exit=%d", got)
	}
	if got := ExitCode(InvariantError(StageRemux, "x", 0, "hash")); got != ExitInvariant {
		t.Fatalf("invariant exit=%d", got)
	}
}
