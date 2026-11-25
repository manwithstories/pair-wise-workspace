package timeline

import (
	"errors"
	"testing"
	"time"
)

func TestPCRWrapExtendedMonotonically(t *testing.T) {
	c := &Clock{}
	near := PCRModulus - 2*PCRHz // two seconds before wrap
	d0, err := c.FeedPCR(near)
	if err != nil {
		t.Fatal(err)
	}
	d1, err := c.FeedPCR(PCRHz) // one second after zero
	if err != nil {
		t.Fatal(err)
	}
	if c.WrapCount() != 1 {
		t.Fatalf("wrap count=%d, want 1", c.WrapCount())
	}
	if got := d1 - d0; got != 3*time.Second {
		t.Fatalf("duration across wrap=%s, want 3s", got)
	}
}

func TestPCRRejectsOutOfRange(t *testing.T) {
	c := &Clock{}
	if _, err := c.FeedPCR(PCRModulus); err == nil {
		t.Fatal("expected range error")
	} else if !IsUndecidable(err) {
		t.Fatalf("error %v is not classified undecidable", err)
	}
}

func TestInvalidIntervalBlocksClip(t *testing.T) {
	c := &Clock{}
	c.MarkInvalid(40*time.Millisecond, 120*time.Millisecond, "cc-gap-without-pcr")
	if !c.IntersectsInvalid(0, 100*time.Millisecond) {
		t.Fatal("expected intersection")
	}
	err := c.ValidateSpan(0, 200*time.Millisecond)
	if !errors.Is(err, ErrInvalidSpan) {
		t.Fatalf("ValidateSpan=%v, want ErrInvalidSpan", err)
	}
	if err := c.ValidateSpan(120*time.Millisecond, 200*time.Millisecond); err != nil {
		t.Fatalf("clip after invalid interval rejected: %v", err)
	}
}

func TestGapDoesNotImplyInvalid(t *testing.T) {
	c := &Clock{}
	g := c.MarkGap(time.Second, 1040*time.Millisecond, "cc-jump")
	if !g.Recovered {
		t.Fatal("bounded CC gap should retain boundary timing")
	}
	if c.IntersectsInvalid(time.Second, 1040*time.Millisecond) {
		t.Fatal("known bounded gap must not poison the clock")
	}
}
