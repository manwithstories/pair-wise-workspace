package fault

import "testing"

func TestList(t *testing.T) {
	var l List
	if l.HasFatal() || l.FirstOffset() != -1 {
		t.Fatal("empty list")
	}
	l = l.Add(Fault{Stage: StageManifest, Source: "a.m3u8", Offset: 12, Recoverable: true, Message: "truncated"})
	l = l.Add(Fault{Stage: StageFragment, Source: "seg.mp4", Offset: 99, Message: "bad box"})
	if !l.HasFatal() {
		t.Fatal("expected fatal")
	}
	if l.FirstOffset() != 12 {
		t.Fatalf("first offset: got %d", l.FirstOffset())
	}
	if l[0].Error() == "" {
		t.Fatal("error string empty")
	}
}
