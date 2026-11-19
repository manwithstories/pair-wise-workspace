package diag

import (
	"errors"
	"testing"
)

func TestErrorAnnotation(t *testing.T) {
	base := New(PhaseParse, "", "srt.block", "bad timing", true)
	anchored := base.At("clip.srt", 412)
	if anchored.Offset != 412 || anchored.Source != "clip.srt" {
		t.Fatalf("anchor lost: %+v", anchored)
	}
	if base.Offset != OffsetUnknown {
		t.Fatal("receiver mutated by At")
	}
	if !errors.Is(anchored, ErrInvalid) {
		t.Fatal("sentinel not unwrapped")
	}
}

func TestCollector(t *testing.T) {
	c := NewCollector(2)
	if c.Add(New(PhaseParse, "f", "op", "r1", true)) != true {
		t.Fatal("first add should report capacity")
	}
	if c.Add(New(PhaseRead, "f", "op", "r2", false)) != false {
		t.Fatal("limit add should report full")
	}
	c.Add(New(PhaseParse, "f", "op", "r3", true))
	if c.Len() != 3 || c.Recoverable() != 2 {
		t.Fatalf("counts wrong: len=%d rec=%d", c.Len(), c.Recoverable())
	}
	f := c.Fatal()
	if f == nil || f.Message != "r2" {
		t.Fatalf("fatal lookup wrong: %v", f)
	}
}

func TestWrapsCause(t *testing.T) {
	e := Wrap(PhaseVerify, "out.json", "hash", false, ErrChecksum)
	if !errors.Is(e, ErrChecksum) || e.Offset != OffsetUnknown {
		t.Fatal("wrap metadata wrong")
	}
}
