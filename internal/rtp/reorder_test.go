package rtp

import (
	"testing"

	"github.com/mediamend/mediamend/internal/fault"
	"github.com/pion/rtp"
)

func frame(seq, rtps uint64) Frame {
	return Frame{
		Camera: "cam1", FrameSeq: seq, MasterTS: seq * 3000,
		RTPSeq: uint16(rtps), Offset: int64(seq * 100),
	}
}

func push(t *testing.T, r *Reorderer, seqs ...uint64) {
	t.Helper()
	for i, s := range seqs {
		f := frame(uint64(i), s)
		r.Push(f)
	}
}

func TestInOrderPassthrough(t *testing.T) {
	r, err := New(Config{Camera: "cam1"})
	if err != nil {
		t.Fatal(err)
	}
	push(t, r, 10, 11, 12, 13)
	res := r.Finish()
	if len(res.Ordered) != 4 || res.Duplicates != 0 || len(res.Gaps) != 0 || len(res.Faults) != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	for i, f := range res.Ordered {
		if f.RTPSeq != uint16(10+i) {
			t.Fatalf("order[%d]=%d", i, f.RTPSeq)
		}
	}
}

func TestReorderingInsideWindow(t *testing.T) {
	r, _ := New(Config{Camera: "cam1", Window: 8})
	push(t, r, 10, 12, 11, 13)
	res := r.Finish()
	if len(res.Ordered) != 4 {
		t.Fatalf("ordered=%d", len(res.Ordered))
	}
	want := []uint16{10, 11, 12, 13}
	for i, f := range res.Ordered {
		if f.RTPSeq != want[i] {
			t.Fatalf("order=%v want %v", seqOf(res.Ordered), want)
		}
	}
	if len(res.Gaps) != 0 || len(res.Late) != 0 {
		t.Fatalf("reordering within window must not produce gaps/late: gaps=%v late=%v", res.Gaps, res.Late)
	}
}

func seqOf(fs []Frame) []uint16 {
	out := make([]uint16, len(fs))
	for i, f := range fs {
		out[i] = f.RTPSeq
	}
	return out
}

func TestDuplicateSuppressed(t *testing.T) {
	r, _ := New(Config{Camera: "cam1"})
	push(t, r, 5, 6, 6, 7, 7, 8)
	res := r.Finish()
	if len(res.Ordered) != 4 {
		t.Fatalf("ordered=%d want 4 (%v)", len(res.Ordered), seqOf(res.Ordered))
	}
	if res.Duplicates != 2 {
		t.Fatalf("duplicates=%d want 2", res.Duplicates)
	}
	if res.Faults.Count(fault.KindDuplicateFrame) != 2 {
		t.Fatalf("faults=%v", res.Faults)
	}
}

func TestLateFrameIsGapNotInsert(t *testing.T) {
	r, _ := New(Config{Camera: "cam1", Window: 2})
	// 0 is delivered; 1 is missing; the window is forced past 1 by 2,3,4.
	for _, s := range []uint64{0, 2, 3, 4} {
		r.Push(frame(s, s))
	}
	// The missing frame 1 finally arrives, but its slot is long gone.
	r.Push(frame(1, 1))
	res := r.Finish()

	if len(res.Late) != 1 {
		t.Fatalf("late=%v want exactly one late frame", res.Late)
	}
	if res.Late[0].ExtSeq != 1 || res.Late[0].Distance != 4 {
		t.Fatalf("late=%+v", res.Late[0])
	}
	for _, f := range res.Ordered {
		if f.RTPSeq == 1 {
			t.Fatal("late frame was silently inserted into the ordered stream")
		}
	}
	if res.Faults.Count(fault.KindLateFrame) != 1 {
		t.Fatalf("faults=%v", res.Faults)
	}
	if len(res.Gaps) != 1 || res.Gaps[0].From != 1 || res.Gaps[0].To != 1 {
		t.Fatalf("gaps=%+v", res.Gaps)
	}
}

func TestGapThenForcedFlush(t *testing.T) {
	r, _ := New(Config{Camera: "cam1", Window: 3})
	// 0 delivered; 1 missing; 2,3,4 buffered. Window=3 so buffering 5 forces
	// the lowest (2) out, declaring 1 missing.
	for _, s := range []uint64{0, 2, 3, 4, 5} {
		r.Push(frame(s, s))
	}
	res := r.Finish()
	if len(res.Gaps) == 0 {
		t.Fatalf("no gap recorded: %+v", res)
	}
	if res.Gaps[0].From != 1 || res.Gaps[0].To != 1 || res.Gaps[0].Missing != 1 {
		t.Fatalf("gap=%+v want ext seq 1", res.Gaps[0])
	}
	if len(res.Ordered) != 5 {
		t.Fatalf("ordered=%d want 5", len(res.Ordered))
	}
	if seqOf(res.Ordered)[0] != 0 || seqOf(res.Ordered)[1] != 2 {
		t.Fatalf("order=%v", seqOf(res.Ordered))
	}
}

func TestSequenceRollover(t *testing.T) {
	r, _ := New(Config{Camera: "cam1"})
	// 65534, 65535, 0, 1 -> must be delivered in order, one wrap.
	for _, s := range []uint64{65534, 65535, 0, 1} {
		r.Push(frame(s, s))
	}
	res := r.Finish()
	if len(res.Ordered) != 4 {
		t.Fatalf("ordered=%d (%v)", len(res.Ordered), seqOf(res.Ordered))
	}
	want := []uint16{65534, 65535, 0, 1}
	for i, f := range res.Ordered {
		if f.RTPSeq != want[i] {
			t.Fatalf("order=%v want %v", seqOf(res.Ordered), want)
		}
	}
	if res.Wraps != 1 {
		t.Fatalf("wraps=%d want 1", res.Wraps)
	}
	if res.Faults.Count(fault.KindSequenceWrap) != 1 {
		t.Fatalf("faults=%v", res.Faults)
	}
}

func TestSequenceResetNewEpoch(t *testing.T) {
	r, _ := New(Config{Camera: "cam1"})
	for _, s := range []uint64{5000, 5001, 5002} {
		r.Push(frame(s, s))
	}
	// Batch restart: sequence jumps back to 0.
	for _, s := range []uint64{0, 1, 2} {
		r.Push(frame(1000+s, s))
	}
	res := r.Finish()
	if res.Resets != 1 {
		t.Fatalf("resets=%d want 1", res.Resets)
	}
	if len(res.Ordered) != 6 {
		t.Fatalf("ordered=%d want 6", len(res.Ordered))
	}
}

func TestBurstGap(t *testing.T) {
	r, _ := New(Config{Camera: "cam1", Window: 8})
	for _, s := range []uint64{0, 1, 2, 10, 11, 12, 13, 14, 15, 16, 17} {
		r.Push(frame(s, s))
	}
	res := r.Finish()
	var burst uint64
	for _, g := range res.Gaps {
		if g.Missing > 1 {
			burst = g.Missing
		}
	}
	if burst != 7 {
		t.Fatalf("burst gap=%d want 7 (gaps=%+v)", burst, res.Gaps)
	}
	if res.MissingCount() != 7 {
		t.Fatalf("missing=%d", res.MissingCount())
	}
}

func TestParsePacket(t *testing.T) {
	pkt := &rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: 4321, Timestamp: 90000, Marker: true},
		Payload: []byte{1, 2, 3},
	}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	seq, ts, marker, err := ParsePacket(raw)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 4321 || ts != 90000 || !marker {
		t.Fatalf("parsed seq=%d ts=%d marker=%v", seq, ts, marker)
	}
	if _, _, _, err := ParsePacket([]byte{1}); err == nil {
		t.Fatal("malformed packet accepted")
	}
}
