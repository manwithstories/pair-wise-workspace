package fault

import (
	"errors"
	"testing"
)

func TestErrorString(t *testing.T) {
	e := New(CodeTruncated, "v0", 12345, true, "eof at packet 9")
	want := "TRUNCATED_MEDIA track=v0 tick=12345: eof at packet 9"
	if e.Error() != want {
		t.Fatalf("got %q want %q", e.Error(), want)
	}
}

func TestOf(t *testing.T) {
	if Of(nil) != nil {
		t.Fatal("nil should map to nil")
	}
	if Of(errors.New("boom")) != nil {
		t.Fatal("plain error is not a fault")
	}
	fe := New(CodeMissingTrack, "a0", -1, false, "no audio")
	if Of(fe) != fe {
		t.Fatal("fault not extracted")
	}
	if !IsFault(fe) || IsFault(errors.New("x")) {
		t.Fatal("IsFault misclassified")
	}
}
