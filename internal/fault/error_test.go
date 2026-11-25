package fault

import (
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	orig := New(KindBlocked, "decode:input-4", "dependency failed").WithBlockedBy("fetch:input-4")
	data, err := Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	back, ok := Unmarshal(data).(*Error)
	if !ok {
		t.Fatal("unmarshal did not yield *Error")
	}
	if back.Kind != KindBlocked || back.Node != "decode:input-4" {
		t.Fatalf("got %+v", back)
	}
	if len(back.BlockedBy) != 1 || back.BlockedBy[0] != "fetch:input-4" {
		t.Fatalf("blocked_by lost: %+v", back.BlockedBy)
	}
}

func TestNilAndPlainError(t *testing.T) {
	if err := Unmarshal(must(Marshal(nil))); err != nil {
		t.Fatalf("nil round trip: %v", err)
	}
	if KindOf(nil) != "" {
		t.Fatal("KindOf(nil) must be empty")
	}
}

func TestMessage(t *testing.T) {
	e := New(KindConflict, "reorder", "seq %d payload mismatch", 42)
	if !strings.Contains(e.Error(), "conflict_packet") || !strings.Contains(e.Error(), "42") {
		t.Fatalf("bad message: %s", e.Error())
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
