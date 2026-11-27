package fault

import (
	"errors"
	"fmt"
	"testing"
)

func TestExitCodes(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{New(Unsupported, "wav.decode", 12, "truncated header"), 2},
		{New(Unrecovered, "nettrace.recover", -1, "gap at seq 9"), 3},
		{New(Corrupt, "cache.verify", 0, "digest mismatch"), 1},
		{errors.New("boom"), 1},
	}
	for i, c := range cases {
		if got := ExitCode(c.err); got != c.want {
			t.Fatalf("case %d: got %d want %d", i, got, c.want)
		}
	}
}

func TestWrappedAndLocatable(t *testing.T) {
	base := New(Unsupported, "wav.layout", 7, "unknown layout tag 0x7f")
	wrapped := fmt.Errorf("track 2: %w", base)
	if !IsCode(wrapped, Unsupported) {
		t.Fatal("IsCode should see through wrapping")
	}
	if ExitCode(wrapped) != 2 {
		t.Fatal("wrapped unsupported must exit 2")
	}
	var e *Error
	if !errors.As(wrapped, &e) || e.Pos != 7 {
		t.Fatalf("position lost: %+v", e)
	}
	if got := base.Error(); got != "wav.layout @7: unknown layout tag 0x7f" {
		t.Fatalf("message %q", got)
	}
}
