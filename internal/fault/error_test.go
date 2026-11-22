package fault

import (
	"errors"
	"io"
	"testing"
)

func TestErrorFields(t *testing.T) {
	err := At(KindCorruptInput, "parse", 4096, "truncated NAL unit")
	fe, ok := As(err)
	if !ok || fe.Offset != 4096 || fe.Kind != KindCorruptInput || fe.Stage != "parse" {
		t.Fatalf("unexpected: %+v", fe)
	}
	if s := err.Error(); s == "" || fe.Kind.String() != "corrupt_input" {
		t.Fatalf("bad message %q or kind", s)
	}
}

func TestWithTrack(t *testing.T) {
	err := WithTrack(At(KindChecksum, "cache", 12, "digest mismatch"), 7)
	fe, _ := As(err)
	if fe.Track != 7 || fe.Offset != 12 || fe.Kind != KindChecksum {
		t.Fatalf("annotation lost fields: %+v", fe)
	}
	plain := errors.New("plain")
	if WithTrack(plain, 1) != plain {
		t.Fatal("non-fault errors must be returned untouched")
	}
}

func TestWrap(t *testing.T) {
	base := At(KindCorruptInput, "parse", 3, "bad byte")
	wrapped := Wrap(base, "pipeline")
	fe, _ := As(wrapped)
	if fe.Stage != "pipeline/parse" || fe.Kind != KindCorruptInput {
		t.Fatalf("wrap changed semantics: %+v", fe)
	}
	if !errors.Is(wrapped, base) {
		t.Fatal("wrapped chain must preserve the base error")
	}
	if Wrap(nil, "x") != nil {
		t.Fatal("Wrap(nil) must be nil")
	}
	fe2, _ := As(Wrap(io.EOF, "store"))
	if fe2.Kind != KindIO {
		t.Fatalf("plain errors should default to IO, got %v", fe2.Kind)
	}
}
