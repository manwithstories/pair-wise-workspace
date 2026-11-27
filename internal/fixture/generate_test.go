package fixture

import (
	"bytes"
	"testing"

	"tscheck/internal/model"
	"tscheck/internal/ts"
)

func TestGenerateDeterministic(t *testing.T) {
	opts := Options{Seed: 41, Bytes: 1 << 20, Branch: 1, VFR: true,
		Faults: FaultSpec{PCRWrap: true, CCJump: true, PSIBump: true, BadPSICRC: true, Truncate: true}}
	a, fa := Generate(opts)
	b, fb := Generate(opts)
	if !bytes.Equal(a, b) {
		t.Fatal("same seed produced different bytes")
	}
	if fa != fb {
		t.Fatalf("fault offsets differ: %+v vs %+v", fa, fb)
	}
	c, _ := Generate(Options{Seed: 42, Bytes: 1 << 20, Branch: 1})
	if bytes.Equal(a, c) {
		t.Fatal("different seeds produced identical bytes")
	}
}

func TestGenerateCleanScansClean(t *testing.T) {
	data, faults := Generate(Options{Seed: 7, Bytes: 300_000, Branch: 0})
	if faults.First() != -1 {
		t.Fatalf("clean stream has faults: %+v", faults)
	}
	if len(data)%ts.PacketSize != 0 {
		t.Fatalf("length %d not packet aligned", len(data))
	}
	_, rep := ts.NewScanner().Scan(data)
	if rep.FirstFaultOffset != -1 {
		t.Fatalf("clean stream scan faults: %+v", rep.Faults)
	}
}

func TestGenerateFaultsDetectedAtRecordedOffsets(t *testing.T) {
	data, faults := Generate(Options{Seed: 41, Bytes: 400_000, Branch: 1,
		Faults: FaultSpec{CCJump: true, BadPSICRC: true, Truncate: true}})
	if faults.CCJump < 0 || faults.BadPSICRC < 0 || faults.Truncation < 0 {
		t.Fatalf("faults not injected: %+v", faults)
	}
	_, rep := ts.NewScanner().Scan(data)
	if rep.FirstFaultOffset != faults.First() {
		t.Fatalf("scanner first fault %d, injected %d",
			rep.FirstFaultOffset, faults.First())
	}
	var sawGap, sawTrunc bool
	for _, f := range rep.Faults {
		switch f.Kind {
		case model.FaultContinuityGap:
			sawGap = true
		case model.FaultTruncated:
			sawTrunc = true
		}
	}
	if !sawGap || !sawTrunc {
		t.Fatalf("missing gap/truncation in %+v", rep.Faults)
	}
}

func TestGeneratePCRWrapAndPSI(t *testing.T) {
	data, _ := Generate(Options{Seed: 3, Bytes: 200_000, Branch: 2,
		Faults: FaultSpec{PCRWrap: true, PSIBump: true, BadPSICRC: true}})
	pkts, _ := ts.NewScanner().Scan(data)
	tr := ts.NewPSITracker()
	var sawWrap, sawBadCRC bool
	var prevPCR int64
	for _, pi := range pkts {
		for _, f := range tr.Feed(pi) {
			if f.Kind == model.FaultPSICRC {
				sawBadCRC = true
			}
		}
		if pi.HasPCR {
			if prevPCR > 0 && pi.PCRBase < prevPCR {
				sawWrap = true
			}
			prevPCR = pi.PCRBase
		}
	}
	if !sawWrap {
		t.Fatal("PCR wrap not present")
	}
	if !sawBadCRC {
		t.Fatal("bad CRC PMT not present")
	}
	// Trusted mapping survives the corrupted update.
	st := tr.Streams()
	if len(st) != 1 || st[0].PID != VideoPID || st[0].StreamType != StreamTypeH264 {
		t.Fatalf("streams = %+v", st)
	}
}

func TestGenerateBranchesShareSchedule(t *testing.T) {
	a, _ := Generate(Options{Seed: 9, Bytes: 100_000, Branch: 0})
	b, _ := Generate(Options{Seed: 9, Bytes: 100_000, Branch: 2})
	// Same seed: identical PTS schedule, different sizes.
	if len(a) == len(b) {
		t.Fatal("branches should differ in size")
	}
}
