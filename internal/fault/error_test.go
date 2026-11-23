package fault_test

import (
	"errors"
	"testing"

	"media-pipeline/internal/fault"
)

func TestStructuredError(t *testing.T) {
	base := errors.New("io: short write")
	fe := fault.New(fault.KindMalformedInput, "src-7", 128, 32, true, "bad riff").
		WithOp("wav.decode").Wrap(base)
	s := fe.Error()
	for _, want := range []string{"wav.decode", "src-7", "byte=128", "sample=32", "recoverable=true", "short write"} {
		if !contains(s, want) {
			t.Fatalf("error %q missing %q", s, want)
		}
	}
	if !errors.Is(fe, base) {
		t.Fatal("errors.Is failed to reach wrapped error")
	}
	got, ok := fault.As(fe)
	if !ok || got.Kind != fault.KindMalformedInput {
		t.Fatal("fault.As failed")
	}
}

func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		kind fault.Kind
		want int
	}{
		{fault.KindMalformedInput, fault.ExitInput},
		{fault.KindGap, fault.ExitInput},
		{fault.KindCheckpointConflict, fault.ExitCheckpointFault},
		{fault.KindOther, fault.ExitInternal},
	}
	for _, c := range cases {
		err := fault.New(c.kind, "", -1, -1, false, "x")
		if got := fault.ExitCode(err); got != c.want {
			t.Fatalf("kind %d -> %d, want %d", c.kind, got, c.want)
		}
	}
	if got := fault.ExitCode(nil); got != fault.ExitOK {
		t.Fatalf("nil -> %d, want 0", got)
	}
	if got := fault.ExitCode(errors.New("plain")); got != fault.ExitInternal {
		t.Fatalf("plain error -> %d, want 1", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
