package fixture

import (
	"testing"

	rtpwin "mediacheck/internal/rtp"
)

func TestRTPBurstSplitReorderAndHardLoss(t *testing.T) {
	cfg := DefaultConfig() // broadcast, LossBurst=12
	pkg, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Every emitted sequence must be unique: no packet sent twice.
	seen := map[uint16]int{}
	for _, p := range pkg.RTPPackets {
		seen[p.SequenceNumber]++
	}
	for seq, n := range seen {
		if n != 1 {
			t.Fatalf("sequence %d emitted %d times, want exactly once", seq, n)
		}
	}

	win, err := rtpwin.NewWindow(64)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pkg.RTPPackets {
		if _, err := win.Push(p); err != nil {
			t.Fatal(err)
		}
	}
	win.Flush()
	m := win.Metrics()

	if m.Duplicates != 0 {
		t.Fatalf("no duplicates expected, got %d", m.Duplicates)
	}
	if m.GapPackets != cfg.LossBurst {
		t.Fatalf("gap packets=%d want %d", m.GapPackets, cfg.LossBurst)
	}
	// 11 of 12 restored via reorder, exactly one hard loss remains.
	if m.Recovered != pkg.RTPReorderable {
		t.Fatalf("recovered=%d want %d", m.Recovered, pkg.RTPReorderable)
	}
	if rate := m.RecoveryRate(); rate < 0.90 || rate >= 1.0 {
		t.Fatalf("recovery rate %.4f outside expected (0.90,1.0)", rate)
	}
}

func TestRTPNormalProfileFullyRecoverable(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profile = ProfileNormal
	pkg, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	win, _ := rtpwin.NewWindow(64)
	for _, p := range pkg.RTPPackets {
		if _, err := win.Push(p); err != nil {
			t.Fatal(err)
		}
	}
	win.Flush()
	if rate := win.Metrics().RecoveryRate(); rate != 1.0 {
		t.Fatalf("normal profile must fully recover, got %.4f", rate)
	}
}
