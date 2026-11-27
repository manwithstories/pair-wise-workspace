package fault

import (
	"errors"
	"fmt"
	"testing"
)

func TestFaultPreservesOffset(t *testing.T) {
	f := New(Truncated, 41, "tail cut inside record %d", 3)
	if f.Offset() != 41 {
		t.Fatalf("offset = %d, want 41", f.Offset())
	}
	if got, want := f.Error(), "truncated fault at offset 41: tail cut inside record 3"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestFaultNegativeOffsetClampsToZero(t *testing.T) {
	if f := New(CRC, -9, "bad"); f.Offset() != 0 {
		t.Fatalf("offset = %d, want 0", f.Offset())
	}
}

func TestFaultOffsetIsNotRewritableByDownstreamWrap(t *testing.T) {
	base := New(CRC, 4096, "payload crc mismatch")
	tracked := base.WithTrack(2).WithLength(64)

	wrapped := fmt.Errorf("stage 2: %w", tracked)
	f, ok := As(wrapped)
	if !ok {
		t.Fatalf("fault not found in chain")
	}
	if f.Offset() != 4096 {
		t.Fatalf("offset drifted to %d", f.Offset())
	}
	if f.Track() != 2 || f.Length() != 64 {
		t.Fatalf("annotation lost: track=%d len=%d", f.Track(), f.Length())
	}
}

func TestWrapKeepsCauseAndKind(t *testing.T) {
	root := errors.New("unexpected EOF")
	f := Wrap(Truncated, 128, root, "record header cut")
	if !errors.Is(f, root) {
		t.Fatalf("cause not reachable")
	}
	if !IsKind(f, Truncated) {
		t.Fatalf("kind lost")
	}
	if IsKind(f, CRC) {
		t.Fatalf("matched wrong kind")
	}
}

func TestKindStrings(t *testing.T) {
	cases := map[Kind]string{
		Truncated: "truncated", CRC: "crc", Length: "length", Index: "index",
		Boundary: "boundary", Limit: "limit", Canceled: "canceled",
		Checkpoint: "checkpoint", Verify: "verify", Kind(99): "unknown",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Fatalf("Kind(%d) = %q, want %q", k, got, want)
		}
	}
}

func TestAsOnPlainError(t *testing.T) {
	if _, ok := As(errors.New("plain")); ok {
		t.Fatalf("plain error should not yield a fault")
	}
	if f, ok := As(DefaultError(Limit, "too many tracks")); !ok || f.Kind() != Limit {
		t.Fatalf("default error not recognised")
	}
}
