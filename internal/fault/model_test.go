package fault

import "testing"

func TestListSortedAndRecoverable(t *testing.T) {
	var l List
	l.Add(Fault{Track: "v1", Frame: 7, Kind: KindTruncated, Recoverable: true})
	l.Add(Fault{Track: "a1", Frame: -1, Kind: KindUnsupportedLayout})
	l.Add(Fault{Track: "v1", Frame: 3, Kind: KindHashMismatch, Recoverable: true})
	items := l.Items()
	if len(items) != 3 {
		t.Fatalf("want 3 items, got %d", len(items))
	}
	if items[0].Track != "a1" || items[1].Frame != 3 || items[2].Frame != 7 {
		t.Fatalf("bad order: %+v", items)
	}
	if !l.HasUnrecoverable() {
		t.Fatal("expected unrecoverable fault")
	}
}

func TestString(t *testing.T) {
	f := Fault{Track: "v1", Frame: 2, Bytes: ByteRange{Start: 10, End: 20},
		Kind: KindTruncated, Recoverable: true, Detail: "short IEND"}
	got := f.String()
	want := "truncated track=v1 frame=2 bytes=[10,20) recoverable=true: short IEND"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
