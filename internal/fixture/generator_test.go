package fixture

import (
	"testing"
)

func base() Config {
	return Config{
		SSRC:          0xC0FFEE01,
		Packets:       500,
		IntervalMs:    1,
		ReorderWindow: 32,
		ReorderRate:   0.05,
		DuplicateRate: 0.02,
		LossRate:      0.01,
		Seed:          7,
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	a, err := Build(base())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(base())
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Send) != len(b.Send) {
		t.Fatalf("schedule length differs: %d vs %d", len(a.Send), len(b.Send))
	}
	for i := range a.Send {
		if a.Send[i] != b.Send[i] {
			t.Fatalf("schedule differs at %d: %+v vs %+v", i, a.Send[i], b.Send[i])
		}
	}
	if a.Expected != b.Expected {
		t.Fatalf("expected counts differ: %+v vs %+v", a.Expected, b.Expected)
	}
}

func TestSeedChangesSchedule(t *testing.T) {
	a, _ := Build(base())
	cfg := base()
	cfg.Seed = 99
	b, _ := Build(cfg)
	if len(a.Send) == len(b.Send) {
		same := true
		for i := range a.Send {
			if a.Send[i] != b.Send[i] {
				same = false
				break
			}
		}
		if same {
			t.Fatal("a different seed produced an identical schedule")
		}
	}
}

func TestSequenceWrapsWithinPlan(t *testing.T) {
	cfg := base()
	cfg.Packets = 600
	cfg.StartSequence = 65200
	cfg.LossRate = 0
	plan, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var wrapped bool
	for _, p := range plan.Send {
		if p.Sequence < cfg.StartSequence {
			wrapped = true
			break
		}
	}
	if !wrapped {
		t.Fatal("plan did not cross the 16-bit sequence boundary")
	}
}

func TestLossDuplicatesAndReordersMatchPrediction(t *testing.T) {
	plan, err := Build(base())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Expected.Dropped == 0 {
		t.Fatal("1% loss over 500 packets should drop something")
	}
	if plan.Expected.Duplicated == 0 {
		t.Fatal("2% duplicate rate over 500 packets should duplicate something")
	}
	if plan.Expected.Reordered == 0 {
		t.Fatal("5% reorder rate should reorder something")
	}
	if plan.Expected.Sent != len(plan.Send) {
		t.Fatalf("Sent=%d but schedule has %d entries", plan.Expected.Sent, len(plan.Send))
	}
	if plan.Expected.Unique != plan.Source-plan.Expected.Dropped {
		t.Fatalf("unique %d, source %d, dropped %d are inconsistent",
			plan.Expected.Unique, plan.Source, plan.Expected.Dropped)
	}
	if plan.Expected.Duplicated != plan.Expected.Sent-plan.Expected.Unique {
		t.Fatalf("duplicate count inconsistent: %+v", plan.Expected)
	}
}

func TestScheduleIsTimeOrdered(t *testing.T) {
	plan, _ := Build(base())
	for i := 1; i < len(plan.Send); i++ {
		if plan.Send[i].OffsetMs < plan.Send[i-1].OffsetMs {
			t.Fatalf("schedule out of order at %d: %d < %d",
				i, plan.Send[i].OffsetMs, plan.Send[i-1].OffsetMs)
		}
	}
}

func TestDuplicatesReuseOriginalHeader(t *testing.T) {
	plan, _ := Build(base())
	bySeq := map[uint16]SourcePacket{}
	for _, p := range plan.Send {
		if p.Duplicate {
			orig, ok := bySeq[p.Sequence]
			if !ok {
				continue // duplicate whose original was later in the list
			}
			if orig.Timestamp != p.Timestamp || orig.Index != p.Index {
				t.Fatalf("duplicate %d changed its header: %+v vs %+v", p.Sequence, orig, p)
			}
		} else {
			bySeq[p.Sequence] = p
		}
	}
}

func TestTimestampsAdvanceByInterval(t *testing.T) {
	cfg := base()
	cfg.IntervalMs = 2
	cfg.LossRate, cfg.DuplicateRate, cfg.ReorderRate = 0, 0, 0
	plan, _ := Build(cfg)
	want := uint32(180) // 2 ms at 90 kHz
	for i, p := range plan.Send {
		if got := p.Timestamp; got != uint32(i)*want {
			t.Fatalf("packet %d timestamp %d, want %d", i, got, uint32(i)*want)
		}
	}
}

func TestPacketPayloadIsStablePerSequence(t *testing.T) {
	cfg := base()
	cfg.LossRate, cfg.DuplicateRate, cfg.ReorderRate = 0, 0, 0
	plan, _ := Build(cfg)
	first := plan.Send[0].Packet(plan.SSRC)
	again := plan.Send[0].Packet(plan.SSRC)
	if first.SSRC != cfg.SSRC {
		t.Fatalf("packet SSRC %d, want %d", first.SSRC, cfg.SSRC)
	}
	if len(first.Payload) != DefaultPayloadSize {
		t.Fatalf("payload size %d, want %d", len(first.Payload), DefaultPayloadSize)
	}
	for i := range first.Payload {
		if first.Payload[i] != again.Payload[i] {
			t.Fatal("payload is not deterministic")
		}
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Config)
	}{
		{"zero packets", func(c *Config) { c.Packets = 0 }},
		{"zero interval", func(c *Config) { c.IntervalMs = 0 }},
		{"negative window", func(c *Config) { c.ReorderWindow = -1 }},
		{"loss over one", func(c *Config) { c.LossRate = 1.5 }},
		{"negative duplicate", func(c *Config) { c.DuplicateRate = -0.1 }},
		{"reorder over one", func(c *Config) { c.ReorderRate = 2 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mod(&cfg)
			if _, err := Build(cfg); err == nil {
				t.Fatal("expected a configuration error")
			}
		})
	}
}
