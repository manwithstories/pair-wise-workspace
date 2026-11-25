package media

import (
	"errors"
	"testing"
	"time"
)

func TestMediaTimeConversions(t *testing.T) {
	d := 20 * time.Millisecond
	tick := FromDuration(d)
	if tick != MediaTime(1800) {
		t.Fatalf("20ms = %d ticks, want 1800", tick)
	}
	if tick.Duration() != d {
		t.Fatalf("round trip duration = %v, want %v", tick.Duration(), d)
	}
	if got := tick.Milliseconds(); got != 20.0 {
		t.Fatalf("Milliseconds = %v, want 20", got)
	}
}

func TestCheckedAddOverflow(t *testing.T) {
	if _, err := MaxTick.Add(1); !errors.Is(err, ErrTickOverflow) {
		t.Fatalf("max+1 err = %v, want ErrTickOverflow", err)
	}
	if _, err := MediaTime(-MaxTick).Add(-MaxTick); !errors.Is(err, ErrTickOverflow) {
		t.Fatalf("negative overflow not detected")
	}
	if r, err := MediaTime(100).Add(-50); err != nil || r != 50 {
		t.Fatalf("100-50 = %d, %v", r, err)
	}
}

func TestFaultClassification(t *testing.T) {
	err := WrapFault(CodeBoxTruncated, true, "trun cut short", errors.New("unexpected EOF"))
	err = err.WithSegment(3).WithSeq(42, Epoch(2))
	f := AsFault(err)
	if f == nil || f.Code != CodeBoxTruncated || !f.Recoverable || f.Segment != 3 ||
		f.Seq != 42 || f.Epoch != 2 {
		t.Fatalf("fault annotations lost: %+v", f)
	}
	if !IsCode(err, CodeBoxTruncated) || IsCode(err, CodeGap) {
		t.Fatal("IsCode misclassified")
	}
	if !Recoverable(err) {
		t.Fatal("recoverable flag lost")
	}
	if errors.Is(err, ErrTickOverflow) {
		t.Fatal("errors.Is matched unrelated sentinel")
	}
}
