package errors

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrorMessageCarriesLocation(t *testing.T) {
	e := New(KindScan, 3, 17, 4096, true, "sample range past EOF")
	msg := e.Error()
	for _, want := range []string{"scan", "track=3", "sample=17", "offset=4096", "recoverable", "past EOF"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q missing %q", msg, want)
		}
	}
}

func TestUnwrapAndAs(t *testing.T) {
	base := errors.New("underlying")
	e := New(KindIndex, 0, -1, -1, true, "cache").Wrap(base)
	if !errors.Is(e, base) {
		t.Fatal("wrapped cause not exposed")
	}
	var wrapped error = fmt.Errorf("outer: %w", e)
	got, ok := As(wrapped)
	if !ok || got.Kind != KindIndex {
		t.Fatalf("As failed through fmt wrap: %v", got)
	}
	if !IsRecoverable(wrapped) {
		t.Fatal("recoverability lost through wrapping")
	}
}

func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{New(KindManifest, 0, -1, 0, false, "bad"), 1},
		{New(KindConfig, 0, -1, -1, false, "bad"), 1},
		{New(KindTruncated, 1, 5, 10, true, "cut"), 2},
		{New(KindScan, 1, 5, 10, false, "fatal"), 3},
		{New(KindInternal, 0, -1, -1, false, "boom"), 3},
		{errors.New("plain"), 3},
	}
	for _, c := range cases {
		if got := ExitCode(c.err); got != c.want {
			t.Fatalf("ExitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}
