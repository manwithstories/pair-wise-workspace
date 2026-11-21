package clock

import (
	"errors"
	"testing"
)

func frag(seq uint32, tfdt, dur uint64) Frag {
	return Frag{Track: "video", Seq: seq, TFDT: tfdt, TFDTVersion: 1, Duration: dur, Timescale: 90000}
}

func TestSeqGapIsTraceable(t *testing.T) {
	tl := New()
	tl.Add(frag(1, 0, 100))
	tl.Add(frag(2, 100, 100))
	err := tl.Add(frag(4, 200, 100)) // seq 3 missing
	if !errors.Is(err, ErrSeqGap) {
		t.Fatalf("want ErrSeqGap, got %v", err)
	}
	if len(tl.Issues) != 1 || tl.Issues[0].Kind != "seq-gap" {
		t.Fatalf("issue not recorded: %+v", tl.Issues)
	}
	// The fragment is still recorded at its stated time.
	if got := len(tl.Frags("video")); got != 3 {
		t.Fatalf("frags=%d want 3", got)
	}
}

func TestDuplicateSeqRejected(t *testing.T) {
	tl := New()
	tl.Add(frag(1, 0, 100))
	if err := tl.Add(frag(1, 100, 100)); !errors.Is(err, ErrDupSeq) {
		t.Fatalf("want ErrDupSeq, got %v", err)
	}
}

func TestTFDTRollbackKeptAsStated(t *testing.T) {
	tl := New()
	tl.Add(frag(1, 0, 100))
	err := tl.Add(frag(2, 50, 100)) // expected 100
	if !errors.Is(err, ErrTFDTRollback) {
		t.Fatalf("want ErrTFDTRollback, got %v", err)
	}
	if tl.Rollbacks != 1 {
		t.Fatalf("rollbacks=%d want 1", tl.Rollbacks)
	}
	// Not silently rewritten: the stated tfdt 50 is what we keep.
	fr := tl.Frags("video")
	if fr[0].TFDT != 50 || fr[1].TFDT != 0 {
		// Frags sorts by tfdt: stated values must survive.
		if fr[0].TFDT != 0 || fr[1].TFDT != 50 {
			t.Fatalf("tfdt rewritten: %+v", fr)
		}
	}
}

func TestTFDTOverflow32Bit(t *testing.T) {
	tl := New()
	f := frag(1, 1<<32-10, 100)
	f.TFDTVersion = 0
	if err := tl.Add(f); !errors.Is(err, ErrTFDTOverflow) {
		t.Fatalf("want ErrTFDTOverflow, got %v", err)
	}
}

func TestTFDTOverflow64Carry(t *testing.T) {
	tl := New()
	f := frag(1, ^uint64(0)-5, 100) // 64-bit carry
	if err := tl.Add(f); !errors.Is(err, ErrTFDTOverflow) {
		t.Fatalf("want ErrTFDTOverflow, got %v", err)
	}
}

func TestSummaryDeterministic(t *testing.T) {
	build := func() *Timeline {
		tl := New()
		tl.Add(frag(1, 0, 100))
		tl.Add(frag(2, 100, 100))
		tl.Add(Frag{Track: "audio", Seq: 1, TFDT: 15, TFDTVersion: 1, Duration: 480, Timescale: 48000})
		return tl
	}
	if build().Summary() != build().Summary() {
		t.Fatal("identical input produced different summaries")
	}
	other := New()
	other.Add(frag(1, 0, 100))
	if other.Summary() == build().Summary() {
		t.Fatal("different input produced identical summaries")
	}
}

func TestToMs(t *testing.T) {
	if got := ToMs(45000, 90000); got != 500 {
		t.Fatalf("ToMs=%v want 500", got)
	}
	if got := ToMs(100, 0); got != 0 {
		t.Fatalf("zero timescale: ToMs=%v want 0", got)
	}
}
