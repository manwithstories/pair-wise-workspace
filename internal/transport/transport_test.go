package transport

import (
	"bytes"
	"testing"
)

func mustPacket(t *testing.T, seq uint16, payload []byte) []byte {
	t.Helper()
	b, err := BuildRTPPacket(seq, uint32(seq)*3000, 41, seq%2 == 0, payload)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReorderDedupAndSequenceWrap(t *testing.T) {
	w, err := NewReorderWindow(64)
	if err != nil {
		t.Fatal(err)
	}
	arrival := []uint16{65534, 0, 65535, 0, 1}
	var delivered []int64
	for _, seq := range arrival {
		ready, err := w.AcceptBytes(mustPacket(t, seq, []byte("x")))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ready {
			delivered = append(delivered, p.ExtendedSequence)
		}
	}
	want := []int64{65534, 65535, 65536, 65537}
	if len(delivered) != len(want) {
		t.Fatalf("delivered=%v want %v", delivered, want)
	}
	for i := range want {
		if delivered[i] != want[i] {
			t.Fatalf("delivered=%v want %v", delivered, want)
		}
	}
	if w.Counters.DupDrop != 1 {
		t.Fatalf("dup_drop=%d want 1", w.Counters.DupDrop)
	}
	if w.Counters.Reordered == 0 {
		t.Fatal("expected reordered packet count")
	}
}

func TestBurstLossRecordsExactGap(t *testing.T) {
	w, _ := NewReorderWindow(64)
	for seq := uint16(10); seq <= 20; seq++ {
		if seq >= 13 && seq <= 15 {
			continue
		}
		if _, err := w.AcceptBytes(mustPacket(t, seq, nil)); err != nil {
			t.Fatal(err)
		}
	}
	final := w.Finish()
	if len(final) != 5 { // 16..20; 10..12 were already delivered
		t.Fatalf("final packets=%d want 5", len(final))
	}
	if w.Counters.GapPackets != 3 {
		t.Fatalf("gaps=%d want 3", w.Counters.GapPackets)
	}
	if len(w.Gaps) != 1 || w.Gaps[0].Start != 13 || w.Gaps[0].End != 15 {
		t.Fatalf("gap ledger=%+v want 13..15", w.Gaps)
	}
}

func TestBoundedWindowOverflowDoesNotBufferForever(t *testing.T) {
	w, _ := NewReorderWindow(4)
	if _, err := w.AcceptBytes(mustPacket(t, 0, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.AcceptBytes(mustPacket(t, 10, nil)); err != nil {
		t.Fatal(err)
	}
	if len(w.buf) > w.Size {
		t.Fatalf("buffered %d exceeds window %d", len(w.buf), w.Size)
	}
	if w.Counters.WindowOverflows != 1 || w.Counters.GapPackets == 0 {
		t.Fatalf("overflow stats=%+v gaps=%+v", w.Counters, w.Gaps)
	}
	if w.Gaps[0].Start != 1 || w.Gaps[0].End != 6 {
		t.Fatalf("overflow gap=%+v want 1..6", w.Gaps[0])
	}
}

func TestWindowOverflowDoesNotMarkBufferedPacketsMissing(t *testing.T) {
	w, _ := NewReorderWindow(4)
	mustAccept := func(seq uint16) []OrderedPacket {
		ready, err := w.AcceptBytes(mustPacket(t, seq, nil))
		if err != nil {
			t.Fatal(err)
		}
		return ready
	}
	mustAccept(0)
	mustAccept(2) // buffered behind missing 1
	mustAccept(3)
	ready := mustAccept(10)
	gotSeqs := make([]int64, 0, len(ready))
	for _, p := range ready {
		gotSeqs = append(gotSeqs, p.ExtendedSequence)
	}
	if len(gotSeqs) != 2 || gotSeqs[0] != 2 || gotSeqs[1] != 3 {
		t.Fatalf("overflow delivery=%v want [2 3]", gotSeqs)
	}
	// Missing forced-forward sequences are 1 and 4,5,6; 2/3 were received.
	if w.Counters.GapPackets != 4 {
		t.Fatalf("gap packets=%d want 4: %+v", w.Counters.GapPackets, w.Gaps)
	}
	if len(w.Gaps) != 2 || w.Gaps[0].Start != 1 || w.Gaps[1].Start != 4 || w.Gaps[1].End != 6 {
		t.Fatalf("gap ranges=%+v", w.Gaps)
	}
}

func TestAssembleReorderedDuplicatedChunks(t *testing.T) {
	payload := bytes.Repeat([]byte("ORAL-HISTORY-"), 40)
	chunks := [][]byte{payload[:100], payload[100:220], payload[220:]}
	seqs := []uint16{40, 42, 41, 40} // chunk 1 arrives after chunk2; seq40 duplicated
	frames := make([][]byte, 0, 4)
	for i, seq := range seqs {
		idx := i
		if i == 1 {
			idx = 2 // sequence 42 carries the final chunk and arrives early
		}
		if i == 2 {
			idx = 1
		}
		if i == 3 {
			idx = 0
		}
		flags := byte(0)
		if idx == 0 {
			flags |= ChunkFlagStart
		}
		if idx == len(chunks)-1 {
			flags |= ChunkFlagEnd
		}
		h := ChunkHeader{Flags: flags, Index: uint16(idx), SegmentID: 9}.Marshal()
		body := append(append([]byte(nil), h...), chunks[idx]...)
		frames = append(frames, mustPacket(t, seq, body))
	}
	segments, win, err := AssembleCapture(frames, 16)
	if err != nil {
		t.Fatal(err)
	}
	if win.Counters.DupDrop != 1 {
		t.Fatalf("dup=%d", win.Counters.DupDrop)
	}
	if len(segments) != 1 || !bytes.Equal(segments[0].Data, payload) {
		t.Fatalf("segments=%d assembled bytes mismatch", len(segments))
	}
}

func TestBandwidthTarget(t *testing.T) {
	r := MeasureBandwidth(1200, []BandwidthSegment{
		{ID: 0, DurationNS: 500_000_000, Bytes: 75_000},
		{ID: 1, DurationNS: 500_000_000, Bytes: 75_000},
	})
	if r.ActualKBPS != 1200 || r.DeviationPct != 0 {
		t.Fatalf("report=%+v", r)
	}
	if !r.WithinTolerance(3) {
		t.Fatal("expected exact bitrate within tolerance")
	}
}
