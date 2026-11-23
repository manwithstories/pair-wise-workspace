package fault

import (
	"errors"
	"strings"
	"testing"
)

func TestErrorCarriesOffsetAndChain(t *testing.T) {
	base := errors.New("short read")
	err := Wrap(base, KindShortRead, "frag-004.m4s", "trun", 4096, "payload exceeds file")
	if !errors.Is(err, base) {
		t.Fatal("underlying error not preserved")
	}
	off, ok := OffsetOf(err)
	if !ok || off != 4096 {
		t.Fatalf("offset = %d, ok = %v", off, ok)
	}
	fe, ok := As(err)
	if !ok || fe.Segment != "frag-004.m4s" || fe.BoxType != "trun" {
		t.Fatalf("fault = %+v", fe)
	}
	msg := err.Error()
	for _, want := range []string{"segment=frag-004.m4s", "box=trun", "offset=4096", "short read"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q missing %q", msg, want)
		}
	}
}

func TestIsKind(t *testing.T) {
	err := New(KindOverflow, "init.mp4", "mdat", 64, "size overflow")
	if !IsKind(err, KindOverflow) || IsKind(err, KindShortRead) {
		t.Fatal("kind matching is wrong")
	}
}
