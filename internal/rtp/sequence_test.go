package rtp

import (
	"errors"
	"testing"

	"github.com/pion/rtp"

	"mediascan/internal/fault"
)

func pkt(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts}}
}

func TestSequenceGapsAndDuplicates(t *testing.T) {
	tr := Track{TrackID: 1, ClockRate: 90_000, Packets: []*rtp.Packet{
		pkt(10, 100), pkt(11, 200), pkt(11, 200), // duplicate
		pkt(14, 500), // missing 12,13
	}}
	rep, err := AnalyzeSequence(tr, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Duplicates != 1 {
		t.Fatalf("duplicates=%d want 1", rep.Duplicates)
	}
	if len(rep.Gaps) != 1 || rep.Gaps[0].Start != 12 || rep.Gaps[0].End != 14 {
		t.Fatalf("gaps wrong: %+v", rep.Gaps)
	}
	if rep.MissingCount() != 2 {
		t.Fatalf("missing=%d want 2", rep.MissingCount())
	}
	fs := SequenceFaults(tr, rep)
	if len(fs) != 1 || fs[0].Kind != fault.KindRTPSeqGap || fs[0].TrackID != 1 {
		t.Fatalf("fault wrong: %+v", fs)
	}
}

func TestSequenceWrapGap(t *testing.T) {
	// Wrap across 16-bit boundary with one missing packet (seq 0 after wrap).
	tr := Track{TrackID: 2, ClockRate: 90_000, Packets: []*rtp.Packet{
		pkt(65534, 100), pkt(65535, 200), pkt(1, 400),
	}}
	rep, err := AnalyzeSequence(tr, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	// Extended range is 65534..65537; 65536 (seq 0) is absent.
	if len(rep.Gaps) != 1 || rep.Gaps[0].Start != 65536 {
		t.Fatalf("wrap gap wrong: %+v", rep.Gaps)
	}
}

func TestReorderNotAFault(t *testing.T) {
	// Every sequence number present, just delivered out of order.
	tr := Track{TrackID: 3, ClockRate: 90_000, Packets: []*rtp.Packet{
		pkt(5, 500), pkt(6, 600), pkt(4, 400), pkt(7, 700),
	}}
	rep, err := AnalyzeSequence(tr, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if rep.MissingCount() != 0 || rep.Duplicates != 0 {
		t.Fatalf("reorder misclassified: %+v", rep)
	}
}

func TestTimestampWrapCountAndAmbiguity(t *testing.T) {
	tr := Track{TrackID: 4, ClockRate: 90_000, Packets: []*rtp.Packet{
		pkt(1, 0xFFFFFFF0),
		pkt(2, 0x00000010), // wrap: raw decrease in send order
		pkt(3, 0x00000030),
	}}
	rep, fs, err := AnalyzeTiming(tr)
	if err != nil {
		t.Fatal(err)
	}
	if rep.WrapCount != 1 {
		t.Fatalf("wrap count=%d want 1", rep.WrapCount)
	}
	if got, ok := fault.FirstByKind(fs, fault.KindRTPWrap); !ok || got.TrackID != 4 {
		t.Fatalf("wrap fault missing: %+v", fs)
	}
}

func TestAmbiguousTimingFault(t *testing.T) {
	tr := Track{TrackID: 5, ClockRate: 90_000, Packets: []*rtp.Packet{
		pkt(1, 0),
		pkt(2, 0x80000000), // 2^31 jump: undecidable
		pkt(3, 0x80000010),
	}}
	_, fs, err := AnalyzeTiming(tr)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fault.FirstByKind(fs, fault.KindAmbiguous); !ok {
		t.Fatalf("ambiguity fault missing: %+v", fs)
	}
}

func TestEmptyAndLimit(t *testing.T) {
	if _, err := AnalyzeSequence(Track{TrackID: 1}, DefaultLimits()); !errors.Is(err, ErrEmptyTrack) {
		t.Fatalf("empty seq err=%v", err)
	}
	if _, _, err := AnalyzeTiming(Track{TrackID: 1}); !errors.Is(err, ErrEmptyTrack) {
		t.Fatalf("empty timing err=%v", err)
	}
	lim := DefaultLimits()
	lim.MaxPackets = 2
	big := Track{TrackID: 1, Packets: []*rtp.Packet{pkt(1, 1), pkt(2, 2), pkt(3, 3)}}
	if _, err := AnalyzeSequence(big, lim); !errors.Is(err, ErrLimit) {
		t.Fatalf("limit err=%v", err)
	}
}
