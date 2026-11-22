package stream

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/pion/rtp"
)

// encode frames one RTP packet with the 2-byte length prefix used on disk.
func encode(t *testing.T, seq uint16, payload []byte) []byte {
	t.Helper()
	pkt := rtp.Packet{
		Header:  rtp.Header{SequenceNumber: seq, SSRC: 42, PayloadType: 96},
		Payload: payload,
	}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, 2+len(raw))
	binary.BigEndian.PutUint16(out, uint16(len(raw)))
	copy(out[2:], raw)
	return out
}

func encodeAll(t *testing.T, seqs ...uint16) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, seq := range seqs {
		buf.Write(encode(t, seq, []byte{0xaa}))
	}
	return buf.Bytes()
}

func TestRTPInOrder(t *testing.T) {
	data := encodeAll(t, 100, 101, 102, 103)
	st, err := AnalyzeRTP(bytes.NewReader(data), "cap.rtp")
	if err != nil {
		t.Fatal(err)
	}
	if st.Packets != 4 || len(st.Gaps) != 0 || st.Duplicates != 0 {
		t.Fatalf("unexpected stats: %+v", st)
	}
	if st.MaxSeq != 103 || st.SSRC != 42 {
		t.Fatalf("maxseq/ssrc wrong: %+v", st)
	}
}

func TestRTPReorderWithinWindow(t *testing.T) {
	// 100, 102, 101: out of order but within the window -> no gap, no dup.
	data := encodeAll(t, 100, 102, 101, 103)
	st, err := AnalyzeRTP(bytes.NewReader(data), "cap.rtp")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Gaps) != 0 || st.OutOfWindow != 0 {
		t.Fatalf("in-window reorder must not create gaps: %+v", st)
	}
}

func TestRTPDuplicateInWindow(t *testing.T) {
	data := encodeAll(t, 100, 101, 101, 102)
	st, err := AnalyzeRTP(bytes.NewReader(data), "cap.rtp")
	if err != nil {
		t.Fatal(err)
	}
	if st.Duplicates != 1 || st.Packets != 3 {
		t.Fatalf("dup handling wrong: packets=%d dups=%d", st.Packets, st.Duplicates)
	}
}

func TestRTPLossOutsideWindow(t *testing.T) {
	// Jump wider than the reorder window: every missing seq becomes a gap.
	seqs := []uint16{100, 100 + DefaultReorderWindow + 5}
	data := encodeAll(t, seqs...)
	st, err := AnalyzeRTP(bytes.NewReader(data), "cap.rtp")
	if err != nil {
		t.Fatal(err)
	}
	if st.OutOfWindow != 0 || len(st.Gaps) != 1 {
		t.Fatalf("expected one jump gap, got %+v", st)
	}
	g := st.Gaps[0]
	// Missing: 101 .. 168 inclusive = 68 packets (169 arrives next).
	if g.From != 101 || g.To != seqs[1]-1 || g.Lost != DefaultReorderWindow+4 {
		t.Fatalf("gap span wrong: %+v", g)
	}
	// Network loss faults must never carry a file offset.
	for _, f := range st.Faults {
		if f.Stage != "rtp" || f.Offset != -1 {
			t.Fatalf("RTP loss fault must be offset-free, got %+v", f)
		}
	}
}

func TestRTPLatePacketOutsideWindow(t *testing.T) {
	// Advance beyond the window, then deliver an old packet: it is late.
	seqs := make([]uint16, 0, DefaultReorderWindow+2)
	for i := uint16(0); i <= DefaultReorderWindow+1; i++ {
		seqs = append(seqs, i)
	}
	data := encodeAll(t, seqs...)
	// Append seq 0 again: base has moved past it.
	data = append(data, encode(t, 0, []byte{0xbb})...)
	st, err := AnalyzeRTP(bytes.NewReader(data), "late.rtp")
	if err != nil {
		t.Fatal(err)
	}
	if st.OutOfWindow != 1 || len(st.LateSeqs) != 1 || st.LateSeqs[0] != 0 {
		t.Fatalf("late packet not recognized: %+v", st)
	}
}

func TestRTPSequenceWrap(t *testing.T) {
	data := encodeAll(t, 65534, 65535, 0, 1)
	st, err := AnalyzeRTP(bytes.NewReader(data), "wrap.rtp")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Gaps) != 0 || st.Packets != 4 {
		t.Fatalf("16-bit wrap must stay continuous: %+v", st)
	}
}

func TestRTPTruncatedRecord(t *testing.T) {
	good := encodeAll(t, 10, 11)
	// Append a length prefix promising 200 bytes followed by only 3 bytes.
	bad := make([]byte, 2)
	binary.BigEndian.PutUint16(bad, 200)
	bad = append(bad, 1, 2, 3)
	st, err := AnalyzeRTP(bytes.NewReader(append(good, bad...)), "trunc.rtp")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range st.Faults {
		if f.Stage == "rtp" && f.Offset >= 0 {
			found = true // malformed/truncated file data keeps its byte offset
		}
	}
	if !found {
		t.Fatalf("truncated record must produce a file-anchored fault, got %+v", st.Faults)
	}
}
