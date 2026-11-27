package fault

import (
	"strings"
	"testing"
)

func TestErrorString(t *testing.T) {
	e := New(KindTruncated, "camB/rep1/seg4.m4s", 1024, true, "tail cut")
	s := e.Error()
	for _, want := range []string{"truncated", "camB/rep1/seg4.m4s", "1024", "true"} {
		if !strings.Contains(s, want) {
			t.Fatalf("Error()=%q missing %q", s, want)
		}
	}
}

func TestFirst(t *testing.T) {
	if First(nil) != nil {
		t.Fatal("nil slice should give nil")
	}
	errs := []*Error{
		New(KindCRC, "a", 500, true, ""),
		nil,
		New(KindTruncated, "b", 120, true, ""),
		New(KindGap, "c", 900, true, ""),
	}
	f := First(errs)
	if f == nil || f.Source != "b" || f.Offset != 120 {
		t.Fatalf("First=%v", f)
	}
}

func TestHasFatal(t *testing.T) {
	if HasFatal([]*Error{New(KindCRC, "a", 0, true, "")}) {
		t.Fatal("recoverable only, want false")
	}
	if !HasFatal([]*Error{New(KindUnknownCodec, "a", 0, false, "")}) {
		t.Fatal("want true for unrecoverable")
	}
}
