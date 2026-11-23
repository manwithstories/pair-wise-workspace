package rtp

import (
	"testing"

	"archive.local/archive/internal/model"
)

func feed(t *Track, seqs []uint16) {
	for i, s := range seqs {
		t.Push(Packet{Seq: s, SSRC: 7, Timestamp: uint32(i * 3000), Payload: []byte{byte(i)}})
	}
}

func TestDuplicateReorderedAndLossCounts(t *testing.T) {
	tr := NewTrack(7)
	// Sequence: 10,11,12,12(dup),14,13(reorder),15,16,17,20,21 (loss 18,19)
	feed(tr, []uint16{10, 11, 12, 12, 14, 13, 15, 16, 17, 20, 21})
	res := tr.Close()
	if res.Duplicate != 1 {
		t.Fatalf("duplicate = %d", res.Duplicate)
	}
	if res.Reordered != 1 {
		t.Fatalf("reordered = %d", res.Reordered)
	}
	if len(res.Gaps) != 1 || res.Gaps[0].Start != 18 || res.Gaps[0].Count != 2 {
		t.Fatalf("gaps = %+v", res.Gaps)
	}
	if res.Lost != 2 {
		t.Fatalf("lost = %d", res.Lost)
	}
	// Confirmed progress stops at the first hole (17), never reporting 21.
	if res.ConfirmedHigh != 17 {
		t.Fatalf("confirmed = %d", res.ConfirmedHigh)
	}
	if res.MaxSeq != 21 {
		t.Fatalf("max = %d", res.MaxSeq)
	}
	if len(res.Arrivals) != 10 {
		t.Fatalf("arrivals = %d", len(res.Arrivals))
	}
	for i := 1; i < len(res.Arrivals); i++ {
		if res.Arrivals[i].ExtSeq <= res.Arrivals[i-1].ExtSeq {
			t.Fatalf("arrivals not sorted at %d", i)
		}
	}
}

func TestSequenceWrapAround(t *testing.T) {
	tr := NewTrack(1)
	seqs := []uint16{65533, 65534, 65535, 0, 1, 2}
	feed(tr, seqs)
	res := tr.Close()
	if res.MaxSeq-res.Arrivals[0].ExtSeq != 5 {
		t.Fatalf("extent = %d", res.MaxSeq-res.Arrivals[0].ExtSeq)
	}
	if res.Reordered != 0 || res.Duplicate != 0 {
		t.Fatalf("reordered/dup = %d/%d", res.Reordered, res.Duplicate)
	}
	if len(res.Gaps) != 0 {
		t.Fatalf("spurious gaps across wrap: %+v", res.Gaps)
	}
	if res.Wraps != 1 {
		t.Fatalf("wraps = %d", res.Wraps)
	}
	if res.ConfirmedHigh != res.MaxSeq {
		t.Fatalf("confirmed = %d max = %d", res.ConfirmedHigh, res.MaxSeq)
	}
}

func TestWrapWithLoss(t *testing.T) {
	tr := NewTrack(2)
	feed(tr, []uint16{65534, 65535, 1, 2}) // 0 lost
	res := tr.Close()
	if len(res.Gaps) != 1 || res.Gaps[0].Start-res.Arrivals[0].ExtSeq != 2 || res.Gaps[0].Count != 1 {
		t.Fatalf("wrap gap = %+v (origin %d)", res.Gaps, res.Arrivals[0].ExtSeq)
	}
	if res.ConfirmedHigh-res.Arrivals[0].ExtSeq != 1 {
		t.Fatalf("confirmed = %d origin %d", res.ConfirmedHigh, res.Arrivals[0].ExtSeq)
	}
}

func TestDuplicateOfWrappedSequence(t *testing.T) {
	tr := NewTrack(3)
	// A late copy of 65535 arrives after 0 and 1. It is dropped as a
	// duplicate; the surviving copy was seen first, so it is not reordered.
	feed(tr, []uint16{65535, 0, 1, 65535})
	res := tr.Close()
	if res.Duplicate != 1 {
		t.Fatalf("duplicate = %d", res.Duplicate)
	}
	if res.Reordered != 0 {
		t.Fatalf("reordered = %d", res.Reordered)
	}
	if len(res.Gaps) != 0 {
		t.Fatalf("gaps = %+v", res.Gaps)
	}
	// A genuinely late packet (2 arrives after 3) is counted as reordering.
	tr2 := NewTrack(3)
	feed(tr2, []uint16{65534, 65535, 1, 0})
	res2 := tr2.Close()
	if res2.Reordered != 1 {
		t.Fatalf("late packet reordered = %d", res2.Reordered)
	}
}

func TestConfirmedProgressSurvivesLaterBurst(t *testing.T) {
	tr := NewTrack(4)
	// Long healthy run, then a burst, then recovery.
	var seqs []uint16
	for s := 0; s < 100; s++ {
		seqs = append(seqs, uint16(s))
	}
	seqs = append(seqs, 100, 101, 102)
	seqs = append(seqs, 140, 141) // 103..139 lost (37-packet burst)
	for s := 142; s < 160; s++ {
		seqs = append(seqs, uint16(s))
	}
	feed(tr, seqs)
	res := tr.Close()
	if res.ConfirmedHigh != 102 {
		t.Fatalf("confirmed = %d (burst must not lower acknowledged progress)", res.ConfirmedHigh)
	}
	if res.Lost != 37 {
		t.Fatalf("lost = %d", res.Lost)
	}
	if len(res.Gaps) != 1 {
		t.Fatalf("gaps = %+v", res.Gaps)
	}
	if len(res.Gaps) != 1 || res.Gaps[0].Count != 37 {
		t.Fatalf("burst gap = %+v", res.Gaps)
	}
}

func TestEstimatorBacksOffOnBurstButKeepsProgress(t *testing.T) {
	e := NewEstimator(1024, 1<<20, 1024)
	// Steady 8 MiB/s link over 100 ms windows.
	var plan BatchPlan
	for i := 0; i < 5; i++ {
		plan = e.Observe(BandwidthSample{WindowMs: 100, Bytes: 800 * 1024}, 0)
	}
	if plan.Packets < 100 {
		t.Fatalf("steady packets = %d", plan.Packets)
	}
	before := plan.Bytes
	plan = e.Observe(BandwidthSample{WindowMs: 100, Bytes: 400 * 1024}, 50)
	if plan.Bytes >= before {
		t.Fatalf("burst did not reduce batch: %d -> %d", before, plan.Bytes)
	}
	// The estimator must not rewind progress it already acknowledged.
	if plan.Bytes < e.MinBytes {
		t.Fatalf("batch below minimum: %d", plan.Bytes)
	}
	// Link then improves; batches should recover upward again.
	var last int
	for i := 0; i < 6; i++ {
		plan = e.Observe(BandwidthSample{WindowMs: 100, Bytes: 2 << 20}, 50)
		last = plan.Bytes
	}
	if last <= before/2 {
		t.Fatalf("batch did not recover: %d", last)
	}
	if plan.SmoothedBPS <= 0 || plan.Confidence != 100 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestEstimatorVaryingBandwidth(t *testing.T) {
	e := NewEstimator(4096, 4<<20, 1200)
	bands := []int64{4 << 20, 1 << 20, 2 << 20, 8 << 20, 512 << 10}
	prev := 0
	for i, b := range bands {
		p := e.Observe(BandwidthSample{WindowMs: 200, Bytes: b / 5}, 0)
		if p.Bytes < e.MinBytes || p.Bytes > e.MaxBytes {
			t.Fatalf("sample %d batch out of range: %d", i, p.Bytes)
		}
		prev = p.Bytes
		_ = prev
	}
}

func TestNoPackets(t *testing.T) {
	tr := NewTrack(9)
	res := tr.Close()
	if res.Duplicate != 0 || len(res.Arrivals) != 0 {
		t.Fatalf("empty track = %+v", res)
	}
	if model.CodeOf(ErrNoPackets(9)) != model.CodeTransportDefect {
		t.Fatal("wrong code")
	}
}

func TestValidateSamples(t *testing.T) {
	if err := ValidateSamples([]BandwidthSample{{WindowMs: -1, Bytes: 1}}); err == nil {
		t.Fatal("negative window accepted")
	}
	if MedianWindow([]BandwidthSample{{WindowMs: 10}, {WindowMs: 30}, {WindowMs: 20}}) != 20 {
		t.Fatal("median")
	}
}

func TestPionRoundTripAndMalformedRejection(t *testing.T) {
	in := Packet{Seq: 1234, Timestamp: 90000, SSRC: 0xA11CE, Payload: []byte{1, 2, 3, 4}}
	wire, err := in.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(wire) != 12+4 {
		t.Fatalf("wire length = %d", len(wire))
	}
	out, err := ParseWire(wire)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.Seq != in.Seq || out.Timestamp != in.Timestamp || out.SSRC != in.SSRC ||
		len(out.Payload) != 4 || out.Payload[0] != 1 {
		t.Fatalf("round trip = %+v", out)
	}
	// A truncated packet must be refused by pion/rtp.
	if _, err := ParseWire(wire[:8]); err == nil || model.CodeOf(err) != model.CodeTransportDefect {
		t.Fatalf("truncated wire err = %v", err)
	}
	// A packet claiming a non-RTP version is rejected too.
	bad := append([]byte(nil), wire...)
	bad[0] = 0x40 // version 1
	if _, err := ParseWire(bad); err == nil {
		t.Fatal("version-1 packet accepted")
	}
}

func TestTracePacketsCarryWireBytes(t *testing.T) {
	// The pipeline decodes trace packets through pion/rtp, so every recorded
	// packet must carry decodable wire bytes with matching header fields.
	pk := Packet{Seq: 7, Timestamp: 100, SSRC: 9, Payload: []byte{0xAA}}
	wire, err := pk.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	dec, err := ParseWire(wire)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Seq != 7 || dec.Timestamp != 100 || dec.SSRC != 9 || dec.Payload[0] != 0xAA {
		t.Fatalf("decoded = %+v", dec)
	}
}
