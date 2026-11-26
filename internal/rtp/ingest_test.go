package rtp

import (
	"strings"
	"testing"
	"time"

	"mediaqc/internal/media"
)

func TestSeqExtenderWrapAndReorder(t *testing.T) {
	var x SeqExtender
	got := []uint64{x.Extend(65534), x.Extend(65535), x.Extend(0), x.Extend(1)}
	want := []uint64{65534, 65535, 65536, 65537}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("seq[%d]=%d want %d (all=%v)", i, got[i], want[i], got)
		}
	}
	if late := x.Extend(65535); late != 65535 {
		t.Fatalf("late pre-wrap seq=%d want 65535", late)
	}
	if ahead := x.Extend(4); ahead != 65540 {
		t.Fatalf("post-wrap seq4=%d want 65540", ahead)
	}
}

func TestIngestDuplicateCountedOnce(t *testing.T) {
	in := NewIngestor()
	raw, err := MakeDatagram(0x1234, 7, 9000, 111, true, []byte("opus"))
	if err != nil {
		t.Fatal(err)
	}
	r1 := in.HandleDatagram(raw, 10*time.Millisecond)
	r2 := in.HandleDatagram(raw, 11*time.Millisecond)
	if r1.Verdict != VerdictAccepted || r2.Verdict != VerdictDuplicate {
		t.Fatalf("verdicts=%s,%s", r1.Verdict, r2.Verdict)
	}
	if in.Stats.Accepted != 1 || in.Stats.Duplicates != 1 {
		t.Fatalf("stats=%+v", in.Stats)
	}
	if r1.Packet.ExtTimestamp != 9000 || r2.Packet.SSRC != 0x1234 {
		t.Fatal("parsed packet fields missing")
	}
}

func TestIngestSameSequenceDifferentPayloadConflict(t *testing.T) {
	in := NewIngestor()
	a, _ := MakeDatagram(1, 42, 100, 96, false, []byte("payload-a"))
	b, _ := MakeDatagram(1, 42, 100, 96, false, []byte("payload-b"))
	if r := in.HandleDatagram(a, 0); r.Verdict != VerdictAccepted {
		t.Fatal(r.Verdict)
	}
	r := in.HandleDatagram(b, time.Millisecond)
	if r.Verdict != VerdictConflict {
		t.Fatalf("verdict=%s err=%v", r.Verdict, r.Err)
	}
	if media.Classify(r.Err) != media.KindConflict || media.ExitCode(r.Err) != 2 {
		t.Fatalf("err classification wrong: %v", r.Err)
	}
	if in.Stats.Conflicts != 1 || in.Stats.Accepted != 1 {
		t.Fatalf("conflicting payload must be isolated: %+v", in.Stats)
	}
}

func TestIngestDistinguishesSSRC(t *testing.T) {
	in := NewIngestor()
	a, _ := MakeDatagram(1, 1, 10, 96, false, []byte("x"))
	b, _ := MakeDatagram(2, 1, 10, 96, false, []byte("x"))
	if in.HandleDatagram(a, 0).Verdict != VerdictAccepted {
		t.Fatal()
	}
	if r := in.HandleDatagram(b, 0); r.Verdict != VerdictAccepted {
		t.Fatalf("different SSRC must not duplicate: %s", r.Verdict)
	}
}

func TestIngestSequenceWrapThroughParser(t *testing.T) {
	in := NewIngestor()
	for _, seq := range []uint16{65534, 65535, 0, 1} {
		raw, err := MakeDatagram(9, seq, uint32(seq)*1800, 111, true, []byte{0x08})
		if err != nil {
			t.Fatal(err)
		}
		r := in.HandleDatagram(raw, 0)
		if r.Verdict != VerdictAccepted {
			t.Fatalf("seq=%d verdict=%s err=%v", seq, r.Verdict, r.Err)
		}
	}
}

func TestIngestTruncatedPacketIsMalformed(t *testing.T) {
	in := NewIngestor()
	// First byte says V=2/CSRC=0; second byte claims extension/padding shape
	// without enough bytes for the declared packet.
	r := in.HandleDatagram([]byte{0x80, 0x80, 0x00}, 0)
	if r.Verdict != VerdictMalformed {
		t.Fatalf("verdict=%s err=%v", r.Verdict, r.Err)
	}
	if media.Classify(r.Err) != media.KindMalformed || media.ExitCode(r.Err) != 2 {
		t.Fatalf("wrong classification: %v", r.Err)
	}
}

func TestIngestOversizedPacketBounded(t *testing.T) {
	in := NewIngestor()
	r := in.HandleDatagram([]byte(strings.Repeat("x", MaxPacketBytes+1)), 0)
	if r.Verdict != VerdictMalformed || in.Stats.Oversized != 1 {
		t.Fatalf("r=%+v stats=%+v", r, in.Stats)
	}
}
