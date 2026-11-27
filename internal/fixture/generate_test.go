package fixture

import (
	"bytes"
	"testing"

	"avflow/internal/model"
	"avflow/internal/stream"
)

func TestCleanFixtureRoundTrip(t *testing.T) {
	g, err := Build(Plan{Seed: 417, Packets: 300, Faults: None()})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.FaultSeqs) != 0 {
		t.Fatalf("clean fixture carries faults: %v", g.FaultSeqs)
	}
	r, err := stream.NewReader(bytes.NewReader(g.Data), 0)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := stream.BuildIndex(r)
	if err != nil {
		t.Fatalf("clean fixture index error: %v", err)
	}
	if len(idx.Faults) != 0 {
		t.Fatalf("clean fixture produced faults: %v", idx.Faults)
	}
	if len(idx.Packets) != 300 {
		t.Fatalf("packets = %d, want 300", len(idx.Packets))
	}
}

func TestDeterministicGeneration(t *testing.T) {
	a, _ := Build(Plan{Seed: 417, Packets: 120, Faults: AllFaults()})
	b, _ := Build(Plan{Seed: 417, Packets: 120, Faults: AllFaults()})
	if !bytes.Equal(a.Data, b.Data) {
		t.Fatal("same seed/plan must produce byte-identical fixtures")
	}
}

func TestAllFaultsInjectedAndLocalized(t *testing.T) {
	g, err := Build(Plan{Seed: 417, Packets: 600, Faults: AllFaults()})
	if err != nil {
		t.Fatal(err)
	}
	want := []model.FaultType{
		model.FaultBadCRC, model.FaultPTSRegression, model.FaultMissingKeyframe,
		model.FaultChannelConflict, model.FaultRateConflict, model.FaultColorConflict,
	}
	for _, ft := range want {
		if _, ok := g.FaultSeqs[ft]; !ok {
			t.Errorf("fault %s not injected", ft)
		}
	}
	// The truncated variant must parse and surface a truncation fault in
	// addition to the earlier injected defects.
	trunc := g.Truncate()
	r, _ := stream.NewReader(bytes.NewReader(trunc), 0)
	idx, err := stream.BuildIndex(r)
	if err == nil {
		t.Fatal("truncated stream must return an error")
	}
	hasTrunc := false
	for _, f := range idx.Faults {
		if f.Type == model.FaultTruncated {
			hasTrunc = true
		}
	}
	if !hasTrunc {
		t.Fatalf("truncation fault not reported: %+v", idx.FirstFault)
	}
	// Bad CRC localizes to the exact injected packet.
	r2, _ := stream.NewReader(bytes.NewReader(g.Data), 0)
	idx2, _ := stream.BuildIndex(r2)
	if idx2.FirstFault == nil {
		t.Fatal("faulty fixture produced no faults")
	}
	injected := g.FaultSeqs[model.FaultBadCRC]
	// First fault should be within 1 packet of an injected defect (precision).
	delta := abs64(int64(idx2.FirstFault.PacketSeq) - int64(injected))
	if idx2.FirstFault.Track == g.Tracks[0].ID && delta > 1 {
		t.Fatalf("localization delta %d exceeds 1 packet", delta)
	}
}

func TestBadCRCPropagatesToCrop(t *testing.T) {
	g, _ := Build(Plan{Seed: 7, Packets: 300, Faults: FaultSet{BadCRC: true}})
	r, _ := stream.NewReader(bytes.NewReader(g.Data), 0)
	idx, err := stream.BuildIndex(r)
	if err != nil {
		t.Fatalf("bad CRC is recoverable, got %v", err)
	}
	if idx.FirstFault == nil || idx.FirstFault.Type != model.FaultBadCRC {
		t.Fatalf("want bad CRC, got %+v", idx.FirstFault)
	}
	if len(idx.ValidPackets()) >= len(idx.Packets) {
		t.Fatal("bad-CRC packet must crop the output prefix")
	}
}

func abs64(v int64) uint64 {
	if v < 0 {
		return uint64(-v)
	}
	return uint64(v)
}
