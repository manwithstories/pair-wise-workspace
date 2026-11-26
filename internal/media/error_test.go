package media

import (
	"errors"
	"strings"
	"testing"
)

func TestErrorClassExitCodes(t *testing.T) {
	cases := []struct {
		err  *Error
		want int
	}{
		{Input("x", "bad"), 2},
		{Integrity("x", "bad"), 3},
		{Interrupted("x", "bad"), 4},
		{E(ClassInternal, "x", "bad"), 1},
	}
	for _, c := range cases {
		if got := ExitCode(c.err); got != c.want {
			t.Fatalf("%s -> exit %d, want %d", c.err.Code, got, c.want)
		}
	}
	if ExitCode(nil) != 0 {
		t.Fatal("nil error must exit 0")
	}
	if ExitCode(errors.New("plain")) != 1 {
		t.Fatal("unknown error must exit 1")
	}
}

func TestAsErrorAndWrap(t *testing.T) {
	if _, ok := AsError(errors.New("plain")); ok {
		t.Fatal("plain error must not classify")
	}
	me, ok := AsError(Wrap(errors.New("root"), ClassIntegrity, "crc.mismatch", "chunk %d", 7))
	if !ok || me.Class != ClassIntegrity || me.Cause == nil {
		t.Fatalf("wrap/as failed: %+v", me)
	}
	if !strings.Contains(me.Error(), "chunk 7") || !strings.Contains(me.Error(), "root") {
		t.Fatalf("message lost detail/cause: %s", me.Error())
	}
}

func TestErrorLocationAnnotation(t *testing.T) {
	e := Input("color.unknown", "unknown color primaries").
		WithStage("video").
		WithPath("clip.png").
		WithField("color.primaries")
	if e.Stage != "video" || e.Path != "clip.png" || e.Field != "color.primaries" {
		t.Fatalf("annotations missing: %+v", e)
	}
	s := e.Error()
	for _, sub := range []string{"[video]", "color.unknown", "color.primaries", "clip.png"} {
		if !strings.Contains(s, sub) {
			t.Fatalf("error string %q missing %q", s, sub)
		}
	}
}
