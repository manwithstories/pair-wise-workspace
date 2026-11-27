package transport

import (
	"testing"

	"mediad/internal/index"
	"mediad/internal/model"
)

func makePackets(n int) []*Packet {
	samples := make([]index.Sample, n)
	for i := range samples {
		samples[i] = index.Sample{
			Index: uint32(i), Size: 100,
			PTS:        model.Timestamp(i) * model.Millisecond,
			DTS:        model.Timestamp(i) * model.Millisecond,
			IsKeyframe: i%5 == 0,
		}
	}
	tr := index.Track{Timescale: 90000, IsVideo: true, Samples: samples}
	return PacketizeTrack(tr)
}

func TestCleanStreamDeliversAll(t *testing.T) {
	pkts := makePackets(20)
	got, gaps, stats, _ := Run(pkts, pkts, DefaultJitterConfig())
	if len(got) != len(pkts) {
		t.Fatalf("delivered %d of %d", len(got), len(pkts))
	}
	if len(gaps) != 0 || stats.RecoveredPackets != 0 || stats.UnrecoveredPackets != 0 {
		t.Fatalf("clean stream must have no loss: %+v gaps=%v", stats, gaps)
	}
	// strict sequence order
	for i := 1; i < len(got); i++ {
		if got[i].RTP.SequenceNumber != got[i-1].RTP.SequenceNumber+1 {
			t.Fatal("output not in sequence order")
		}
	}
}

func TestDeterministicFaultInjection(t *testing.T) {
	pkts := makePackets(40)
	p := &FaultProfile{Seed: 7, ReorderRate: 0.4, DuplicateRate: 0.2, LossRate: 0.1, BurstSize: 2}
	a := NewInjector(p).Inject(pkts)
	b := NewInjector(p).Inject(pkts)
	if len(a) != len(b) {
		t.Fatal("same seed must yield identical stream length")
	}
	for i := range a {
		if a[i].RTP.SequenceNumber != b[i].RTP.SequenceNumber {
			t.Fatalf("non-deterministic impairment at index %d", i)
		}
	}
}

func TestXORRecoversSingleDroppedPacket(t *testing.T) {
	pkts := makePackets(12)
	// drop exactly seq 4: block (4,5) still holds survivor 5 + parity
	impaired := NewInjector(&FaultProfile{DropSeqs: []uint16{4}}).Inject(pkts)
	got, gaps, stats, _ := Run(pkts, impaired, DefaultJitterConfig())
	if stats.RecoveredPackets < 1 {
		t.Fatalf("expected >=1 recovered packet, got %+v", stats)
	}
	if stats.XORRecovered != 1 {
		t.Fatalf("XOR recoveries = %d, want 1", stats.XORRecovered)
	}
	if len(gaps) != 0 || stats.UnrecoveredPackets != 0 {
		t.Fatalf("single drop must be fully recovered: gaps=%v stats=%+v", gaps, stats)
	}
	// reconstructed payload must equal the original exactly
	var rec *Packet
	for _, p := range got {
		if p.RTP.SequenceNumber == 4 {
			rec = p
		}
	}
	if rec == nil {
		t.Fatal("recovered seq 4 missing from output")
	}
	orig := pkts[4]
	if string(rec.RTP.Payload) != string(orig.RTP.Payload) {
		t.Fatal("XOR reconstructed payload differs from original")
	}
}

func TestDoubleLossBecomesTimelineGap(t *testing.T) {
	pkts := makePackets(12)
	// drop both members of one block: XOR impossible, gap must be recorded
	impaired := NewInjector(&FaultProfile{DropSeqs: []uint16{6, 7}}).Inject(pkts)
	got, gaps, stats, _ := Run(pkts, impaired, DefaultJitterConfig())
	if stats.UnrecoveredPackets != 2 {
		t.Fatalf("unrecovered = %d, want 2", stats.UnrecoveredPackets)
	}
	if len(gaps) != 1 || gaps[0].Count != 2 {
		t.Fatalf("expected one 2-packet gap, got %+v", gaps)
	}
	if len(got) != len(pkts)-2 {
		t.Fatalf("delivered %d of %d", len(got), len(pkts)-0)
	}
}

func TestNACKRecoversReorderedPacket(t *testing.T) {
	pkts := makePackets(16)
	// reorder but do not lose: every packet is observable and NACK-satisfiable
	inj := NewInjector(&FaultProfile{Seed: 3, ReorderRate: 0.6})
	impaired := inj.Inject(pkts)
	got, gaps, stats, _ := Run(pkts, impaired, DefaultJitterConfig())
	if len(gaps) != 0 || stats.UnrecoveredPackets != 0 {
		t.Fatalf("reordering alone must not cause loss: gaps=%v stats=%+v", gaps, stats)
	}
	if len(got) != len(pkts) {
		t.Fatalf("delivered %d of %d", len(got), len(pkts))
	}
}

func TestDuplicatesAreDeduplicated(t *testing.T) {
	pkts := makePackets(10)
	inj := NewInjector(&FaultProfile{Seed: 11, DuplicateRate: 0.5})
	impaired := inj.Inject(pkts)
	if len(impaired) <= len(pkts) {
		t.Fatal("injector should have produced duplicates")
	}
	got, _, stats, _ := Run(pkts, impaired, DefaultJitterConfig())
	if len(got) != len(pkts) {
		t.Fatalf("duplicates inflated delivery: %d vs %d", len(got), len(pkts))
	}
	if stats.UnrecoveredPackets != 0 {
		t.Fatalf("duplication is not loss: %+v", stats)
	}
}

func TestNACKCacheSatisfiesRetransmission(t *testing.T) {
	pkts := makePackets(8)
	cache := newNACKCache(64)
	for _, p := range pkts {
		cache.add(p)
	}
	// a NACK for any recently observed sequence is served from the cache
	if got := cache.lookup(2); got == nil || got.RTP.SequenceNumber != 2 {
		t.Fatal("NACK cache must return a retained packet by sequence")
	}
	if got := cache.lookup(99); got != nil {
		t.Fatal("NACK cache miss must return nil")
	}
	// engine-level: a withheld-then-late packet still completes delivery
	eng := NewEngine(DefaultJitterConfig())
	eng.Ingest(pkts)
	for _, p := range pkts {
		if p.RTP.SequenceNumber == 2 {
			continue
		}
		eng.Receive(p)
	}
	eng.Receive(pkts[2]) // late arrival before Finalize
	got, gaps, stats, _ := eng.Finalize()
	if len(gaps) != 0 || len(got) != len(pkts) {
		t.Fatalf("late packet must complete delivery: %+v", gaps)
	}
	if stats.UnrecoveredPackets != 0 {
		t.Fatalf("unrecovered packets: %+v", stats)
	}
}

func TestGapsConvertToSharedFaults(t *testing.T) {
	gaps := []Gap{{StartSeq: 6, EndSeq: 7, PTS: 6000, Count: 2}}
	faults := GapsToFaults(gaps, "cam.mp4")
	if len(faults) != 1 || faults[0].Source != "cam.mp4" {
		t.Fatalf("fault conversion wrong: %+v", faults)
	}
	if faults[0].Offset != model.OffsetUnknown {
		t.Fatal("transport gap carries no byte offset")
	}
}
