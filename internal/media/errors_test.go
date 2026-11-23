package media

import (
	"errors"
	"testing"
)

func TestErrorCarriesTrackAndOffset(t *testing.T) {
	err := Corruptf("manifest.parse", "v1", 4096, "negative sample duration")
	var e *Error
	if !errors.As(err, &e) {
		t.Fatal("expected *Error")
	}
	if e.Track != "v1" || e.Offset != 4096 {
		t.Fatalf("track/offset lost: %+v", e)
	}
	s := e.Error()
	for _, want := range []string{"manifest.parse", "track=v1", "offset=4096"} {
		if !contains(s, want) {
			t.Fatalf("Error() %q missing %q", s, want)
		}
	}
}

func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{Corruptf("op", "t", 0, "bad"), 2},
		{New(KindUnsupported, "op", "", -1, "codec", nil), 2},
		{New(KindCanceled, "op", "", -1, "canceled", nil), 2},
		{New(KindInternal, "op", "", -1, "boom", nil), 1},
		{errors.New("plain"), 1},
	}
	for i, c := range cases {
		if got := ExitCode(c.err); got != c.want {
			t.Fatalf("case %d: ExitCode=%d want %d", i, got, c.want)
		}
	}
}

func TestUnwrap(t *testing.T) {
	cause := errors.New("disk full")
	err := New(KindInternal, "index.commit", "a1", 12, "write failed", cause)
	if !errors.Is(err, cause) {
		t.Fatal("Unwrap broken")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
