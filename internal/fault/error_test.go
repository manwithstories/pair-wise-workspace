package fault

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestClassMapsToExitCode(t *testing.T) {
	cases := map[Class]int{
		ClassInput: 2, ClassDependency: 2, ClassLimit: 2,
		ClassUsage: 1, ClassInternal: 1,
	}
	for class, want := range cases {
		err := New(class, "s", "t", "c", "m")
		if got := ExitCode(err); got != want {
			t.Errorf("class %s: exit=%d want %d", class, got, want)
		}
	}
	if ExitCode(nil) != 0 {
		t.Fatal("nil error exit code")
	}
	if ExitCode(errors.New("plain")) != 1 {
		t.Fatal("plain error should be a usage-style failure")
	}
}

func TestErrorStringCarriesAttribution(t *testing.T) {
	err := New(ClassInput, "frames", "v-main", "checksum-mismatch", "digest differs").WithFrame(7)
	s := err.Error()
	for _, want := range []string{"input error", "stage=frames", "track=v-main", "frame=7", "checksum-mismatch", "digest differs"} {
		if !strings.Contains(s, want) {
			t.Fatalf("%q missing from %q", want, s)
		}
	}
}

func TestWrapAndUnwrap(t *testing.T) {
	cause := errors.New("root")
	err := Wrap(ClassInternal, "dag", "n", "failed", "node failed", cause)
	if !errors.Is(err, cause) {
		t.Fatal("cause not reachable")
	}
	got, ok := As(fmt.Errorf("outer: %w", err))
	if !ok || got.Code != "failed" {
		t.Fatalf("As through wrapping: %v", got)
	}
}

func TestFirstBlockedPicksEarliest(t *testing.T) {
	errs := []error{
		Blocked("dag", "c", "b", 0, 0),
		Blocked("frames", "v", "x", 5, 0),
		Blocked("frames", "v", "y", 2, 0),
	}
	fe := FirstBlocked(errs)
	if fe == nil || fe.Frame != 2 || fe.BlockedBy != "y" {
		t.Fatalf("got %v", fe)
	}
	if got := FirstBlocked([]error{New(ClassInput, "s", "t", "c", "m")}); got != nil {
		t.Fatalf("non-dependency error returned as blocked: %v", got)
	}
}

func TestBlockedDefaults(t *testing.T) {
	err := Blocked("segments", "1600k", "index:1600k", 0, 3)
	if err.Class != ClassDependency || err.BlockedBy != "index:1600k" || err.Segment != 3 {
		t.Fatalf("blocked fields: %+v", err)
	}
}
