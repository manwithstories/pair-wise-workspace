package stream

import (
	"testing"

	"github.com/pion/rtp"

	"mediapipeline/internal/mediaerr"
)

func buildRTP(t *testing.T, seq uint16, ts uint32, ssrc uint32, payload []byte) []byte {
	t.Helper()
	pkt := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
		},
		Payload: payload,
	}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParseRTPRoundTrip(t *testing.T) {
	raw := buildRTP(t, 7, 1000, 42, []byte{1, 2, 3, 4})
	p, merr := ParseRTP(raw, 100, 1)
	if merr != nil {
		t.Fatal(merr)
	}
	if p.Sequence != 7 || p.Timestamp != 1000 || p.SSRC != 42 {
		t.Fatalf("header mismatch: %+v", p)
	}
	if string(p.Payload) != "\x01\x02\x03\x04" || p.Offset != 100 || p.Arrival != 1 {
		t.Fatalf("payload/meta mismatch: %+v", p)
	}
}

func TestParseTruncated(t *testing.T) {
	for n := 0; n < 12; n++ {
		buf := make([]byte, n)
		_, merr := ParseRTP(buf, int64(n), 0)
		if merr == nil || merr.Kind != mediaerr.KindTruncated {
			t.Fatalf("n=%d: expected truncated error, got %v", n, merr)
		}
		if !merr.HasOffset {
			t.Fatalf("n=%d: truncated error must carry offset", n)
		}
	}
}

// rawPaddedRTP hand-builds a padded RTP datagram so invalid padding values
// can be exercised even though pion's Marshal rejects them.
func rawPaddedRTP(ssrc uint32, seq uint16, payload []byte) []byte {
	raw := make([]byte, 12+len(payload))
	raw[0] = 0xA0 // V=2, P=1
	raw[1] = 96
	raw[2] = byte(seq >> 8)
	raw[3] = byte(seq)
	raw[8] = byte(ssrc >> 24)
	raw[9] = byte(ssrc >> 16)
	raw[10] = byte(ssrc >> 8)
	raw[11] = byte(ssrc)
	copy(raw[12:], payload)
	return raw
}

func TestPaddingValidation(t *testing.T) {
	// Padding says 5 but payload is only 2 bytes: pion rejects it first.
	bad := rawPaddedRTP(1, 1, []byte{0xAA, 0x05})
	if _, merr := ParseRTP(bad, 0, 0); merr == nil {
		t.Fatal("expected payload length error for oversized padding")
	}
	// Valid padding: 5 payload bytes with pad count 2, so 3 media bytes
	// remain (the length byte itself counts in the padding).
	good := rawPaddedRTP(1, 1, []byte{0xAA, 0xBB, 0xCC, 0xDD, 0x02})
	p, merr := ParseRTP(good, 0, 0)
	if merr != nil {
		t.Fatal(merr)
	}
	if len(p.Payload) != 3 {
		t.Fatalf("padding not stripped: len=%d", len(p.Payload))
	}
}

func TestTrackerSequenceWrapAndReorder(t *testing.T) {
	tr := NewTracker(9)
	seqs := []uint16{0xFFFE, 0xFFFF, 0x0001, 0x0000} // gap then reorder
	exts := make([]uint64, len(seqs))
	for i, s := range seqs {
		p, merr := ParseRTP(buildRTP(t, s, uint32(s)*160, 9, []byte{byte(s)}), 0, int64(i))
		if merr != nil {
			t.Fatal(merr)
		}
		ev, merr := tr.Observe(p)
		if merr != nil {
			t.Fatal(merr)
		}
		exts[i] = p.ExtSeq
		if i == 2 && ev != SeqGap {
			t.Fatalf("packet 2 should be gap event, got %v", ev)
		}
		if i == 3 && ev != SeqReordered {
			t.Fatalf("packet 3 should be reorder event, got %v", ev)
		}
	}
	if exts[3] != exts[0]+2 { // 0x0000 sits two after 0xFFFE
		t.Fatalf("wrap expansion wrong: %x vs %x", exts[3], exts[0]+2)
	}
	// All four ext values eventually arrived, so final loss is zero; the
	// gap was reported as an event when 0x0001 jumped ahead of 0x0000.
	st := tr.Flush()
	if st.Lost != 0 {
		t.Fatalf("final lost=%d want 0", st.Lost)
	}
	if st.GapEvents != 1 {
		t.Fatalf("gap events=%d want 1", st.GapEvents)
	}
	if st.Reordered != 1 {
		t.Fatalf("reordered=%d want 1", st.Reordered)
	}
}

func TestTrackerDuplicateConflict(t *testing.T) {
	tr := NewTracker(3)
	p1, _ := ParseRTP(buildRTP(t, 10, 100, 3, []byte{1, 2}), 0, 0)
	p2, _ := ParseRTP(buildRTP(t, 10, 100, 3, []byte{9, 9}), 0, 1)
	if _, merr := tr.Observe(p1); merr != nil {
		t.Fatal(merr)
	}
	_, merr := tr.Observe(p2)
	if merr == nil || merr.Kind != mediaerr.KindDuplicate {
		t.Fatalf("expected duplicate conflict, got %v", merr)
	}
	if tr.Stats().Conflicts != 1 || tr.Stats().Duplicates != 1 {
		t.Fatalf("counters wrong: %+v", tr.Stats())
	}
	// Identical duplicate is tolerated and not a conflict.
	p3, _ := ParseRTP(buildRTP(t, 10, 100, 3, []byte{1, 2}), 0, 2)
	if _, merr := tr.Observe(p3); merr != nil {
		t.Fatal(merr)
	}
	if tr.Stats().Conflicts != 1 || tr.Stats().Received != 1 {
		t.Fatalf("identical duplicate miscounted: %+v", tr.Stats())
	}
}

func TestLossCounting(t *testing.T) {
	tr := NewTracker(1)
	for _, s := range []uint16{0, 1, 4} {
		p, _ := ParseRTP(buildRTP(t, s, 0, 1, nil), 0, 0)
		if _, merr := tr.Observe(p); merr != nil {
			t.Fatal(merr)
		}
	}
	if st := tr.Flush(); st.Lost != 2 || st.GapEvents != 2 {
		t.Fatalf("loss stats wrong: %+v", st)
	}
}

func TestRTCPValidation(t *testing.T) {
	// Valid minimal compound: RR with zero report blocks, length word 1.
	good := []byte{0x80, 201, 0x00, 0x01, 0, 0, 0, 0}
	if merr := ParseRTCP(good); merr != nil {
		t.Fatal(merr)
	}
	for _, bad := range [][]byte{
		{0x80},                              // too short
		{0x80, 201, 0x00, 0x09, 0, 0, 0, 0}, // length exceeds datagram
		{0x40, 201, 0x00, 0x01, 0, 0, 0, 0}, // wrong version
	} {
		if merr := ParseRTCP(bad); merr == nil {
			t.Fatalf("expected RTCP error for %x", bad)
		}
	}
	// RFC 5761 RTCP range is PT 64..95 inclusive.
	if !IsRTCP([]byte{0x80, 200}) || !IsRTCP([]byte{0x80, 64}) || !IsRTCP([]byte{0x80, 95}) {
		t.Fatal("RTP/RTCP demux range wrong: 64..95 must be RTCP")
	}
	if IsRTCP([]byte{0x80, 96}) || IsRTCP([]byte{0x80, 33}) {
		t.Fatal("payload types outside 64..95 must demux as RTP")
	}
}
