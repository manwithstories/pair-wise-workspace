package rtp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/archive/tsrestore/internal/timeline"
)

// frame wraps a datagram with the two-byte big-endian length prefix.
func frame(b []byte) []byte {
	out := make([]byte, 2+len(b))
	binary.BigEndian.PutUint16(out, uint16(len(b)))
	copy(out[2:], b)
	return out
}

// rtpDatagram builds a minimal RTP v2 datagram (PT 33, no CSRC/extension).
func rtpDatagram(seq uint16, ts uint32, payload []byte) []byte {
	h := make([]byte, HeaderLen)
	h[0] = 0x80
	h[1] = 33
	binary.BigEndian.PutUint16(h[2:4], seq)
	binary.BigEndian.PutUint32(h[4:8], ts)
	return append(h, payload...)
}

// srDatagram builds a single-block RTCP SR with the given anchor fields.
func srDatagram(ssrc uint32, ntp uint64, rtpTS uint32) []byte {
	b := make([]byte, 28)
	b[0] = 0x80
	b[1] = SRType
	binary.BigEndian.PutUint16(b[2:4], 6) // 7 words - 1
	binary.BigEndian.PutUint32(b[4:8], ssrc)
	binary.BigEndian.PutUint64(b[8:16], ntp)
	binary.BigEndian.PutUint32(b[16:20], rtpTS)
	return b
}

func TestFrameRoundTripAndTruncation(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame([]byte{1, 2, 3, 4}))
	fr := NewFrameReader(&buf)
	got, err := fr.ReadDatagram()
	if err != nil || !bytes.Equal(got, []byte{1, 2, 3, 4}) {
		t.Fatalf("datagram = %v err = %v", got, err)
	}
	if _, err := fr.ReadDatagram(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
	// A prefix promising 10 bytes followed by only 4 must be Corrupt.
	var bad bytes.Buffer
	bad.Write([]byte{0, 10, 1, 2, 3, 4})
	if _, err := NewFrameReader(&bad).ReadDatagram(); !isClass(err, timeline.ClassCorrupt) {
		t.Fatalf("expected corrupt truncation, got %v", err)
	}
}

func TestParsePacketPaddingAndTruncation(t *testing.T) {
	raw := rtpDatagram(7, 99, []byte{0xAA, 0xBB, 0xCC, 0x02})
	raw[0] |= 0x20 // padding bit; last byte says 2 padding bytes
	p, err := ParsePacket(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.Payload, []byte{0xAA, 0xBB}) {
		t.Fatalf("padding-stripped payload = %x", p.Payload)
	}
	if _, err := ParsePacket(raw[:6]); !isClass(err, timeline.ClassCorrupt) {
		t.Fatalf("expected corrupt short packet, got %v", err)
	}
}

func TestParseSenderReport(t *testing.T) {
	b := srDatagram(0x11223344, 5<<32, 900000)
	sr, err := ParseSenderReport(b)
	if err != nil {
		t.Fatal(err)
	}
	if sr.SSRC != 0x11223344 || sr.NTPTime != 5<<32 || sr.RTPTime != 900000 {
		t.Fatalf("SR fields wrong: %+v", sr)
	}
	// Corrupt length word.
	bad := srDatagram(1, 1, 1)
	bad[3] = 99
	if _, err := ParseSenderReport(bad); !isClass(err, timeline.ClassCorrupt) {
		t.Fatalf("expected corrupt SR, got %v", err)
	}
}

func TestReorderWraparoundDuplicateLate(t *testing.T) {
	rb := NewReorderBuffer()
	// Start near the 16-bit ceiling.
	first := rb.Push(mustParse(t, rtpDatagram(0xFFFE, 100, []byte("a"))))
	if len(first) != 1 || first[0].Sequence != 0xFFFE {
		t.Fatalf("first packet anchoring wrong: %d", len(first))
	}
	// Out of order: 0x0001 then 0x0000 (wrap), then a duplicate of 0x0001.
	if out := rb.Push(mustParse(t, rtpDatagram(0x0001, 300, []byte("c")))); len(out) != 0 {
		t.Fatal("0x0001 must wait for 0x0000")
	}
	out := rb.Push(mustParse(t, rtpDatagram(0x0000, 200, []byte("b"))))
	if len(out) != 2 || out[0].Sequence != 0xFFFF || out[1].Sequence != 0x0000 {
		t.Fatalf("wrap ordering wrong: %+v", seqs(out))
	}
	if out := rb.Push(mustParse(t, rtpDatagram(0x0001, 300, []byte("c2")))); len(out) != 1 {
		t.Fatal("held 0x0001 should now release")
	}
	if out := rb.Push(mustParse(t, rtpDatagram(0x0001, 300, []byte("dup")))); len(out) != 0 {
		t.Fatal("duplicate must not release")
	}
	// An ancient late packet (0xFFFE again) is dropped.
	if out := rb.Push(mustParse(t, rtpDatagram(0xFFFE, 100, []byte("late")))); len(out) != 0 {
		t.Fatal("late packet must be dropped")
	}
	st := rb.Stats()
	if st.Duplicates != 1 || st.LateDrop != 1 {
		t.Fatalf("stats wrong: %+v", st)
	}
}

func TestReorderGapDeclaredAtWindowBoundary(t *testing.T) {
	rb := NewReorderBufferWindow(4)
	rb.Push(mustParse(t, rtpDatagram(10, 0, nil))) // released immediately
	// seq 11 lost; 12 and 13 arrive within the window and stay held.
	rb.Push(mustParse(t, rtpDatagram(12, 0, nil)))
	rb.Push(mustParse(t, rtpDatagram(13, 0, nil)))
	if rb.Stats().Gaps != 0 || rb.Pending() != 2 {
		t.Fatalf("gap should stay open inside window: %+v", rb.Stats())
	}
	// seq 14 is windowSize+1 beyond the missing 11: the window is exhausted,
	// gap 11 is declared and surviving packets 12..14 release in order.
	out := rb.Push(mustParse(t, rtpDatagram(14, 0, nil)))
	if len(out) != 3 || seqs(out)[0] != 12 || seqs(out)[2] != 14 {
		t.Fatalf("release after gap wrong: %v", seqs(out))
	}
	if rb.Stats().Gaps != 1 {
		t.Fatalf("gaps = %d, want 1", rb.Stats().Gaps)
	}
}

func TestReorderGapOnFlush(t *testing.T) {
	rb := NewReorderBufferWindow(16)
	rb.Push(mustParse(t, rtpDatagram(10, 0, nil))) // released
	rb.Push(mustParse(t, rtpDatagram(12, 0, nil))) // held, 11 missing
	flushed := rb.Flush()
	if len(flushed) != 1 || flushed[0].Sequence != 12 {
		t.Fatalf("flush order wrong: %v", seqs(flushed))
	}
	if rb.Stats().Gaps != 1 {
		t.Fatalf("gaps = %d, want 1", rb.Stats().Gaps)
	}
}

func TestOrderedReaderMixedStream(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(srDatagram(7, 100<<32, 1000)))
	buf.Write(frame(rtpDatagram(1, 1000, []byte("x"))))
	buf.Write(frame(rtpDatagram(3, 3000, []byte("z")))) // arrives before seq 2
	// Bad RTCP in the middle must be counted, not fatal.
	badSR := srDatagram(7, 1, 1)
	badSR[3] = 50
	buf.Write(frame(badSR))
	buf.Write(frame(rtpDatagram(2, 2000, []byte("y")))) // late but in-window

	r := NewOrderedReader(&buf)
	var srs int
	r.OnSR = func(SenderReport) { srs++ }
	var got []uint16
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		got = append(got, p.Sequence)
	}
	// Reordered seq 2 is recovered inside the window: full order restored.
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("ordered seqs = %v, want [1 2 3]", got)
	}
	if srs != 1 || r.Stats().BadRTCP != 1 {
		t.Fatalf("sr=%d badRTCP=%d", srs, r.Stats().BadRTCP)
	}
	if r.Stats().Gaps != 0 {
		t.Fatalf("gaps = %d, want 0 (reorder recovered, no loss)", r.Stats().Gaps)
	}
}

func mustParse(t *testing.T, b []byte) *Packet {
	t.Helper()
	p, err := ParsePacket(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func seqs(ps []*Packet) []uint16 {
	out := make([]uint16, len(ps))
	for i, p := range ps {
		out[i] = p.Sequence
	}
	return out
}

func isClass(err error, c timeline.Class) bool {
	var te *timeline.Error
	return errors.As(err, &te) && te.Class == c
}
