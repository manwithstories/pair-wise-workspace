package fault

import (
	"errors"
	"testing"
)

func TestErrorLocation(t *testing.T) {
	e := New(KindManifest, Fail, "list.m3u8", 12, 3, "bad EXTINF")
	want := "[manifest/fail] list.m3u8:12 seg#3: bad EXTINF"
	if e.Error() != want {
		t.Fatalf("got %q want %q", e.Error(), want)
	}
}

func TestAs(t *testing.T) {
	var err error = New(KindTransport, FillSilence, "", 0, -1, "gap of 4 packets")
	e, ok := As(err)
	if !ok || e.Kind != KindTransport || e.Recovery != FillSilence {
		t.Fatalf("As failed: %v %v", e, ok)
	}
	if _, ok := As(errors.New("x")); ok {
		t.Fatal("As should fail for plain error")
	}
}
