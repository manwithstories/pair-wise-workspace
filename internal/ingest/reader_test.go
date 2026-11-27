package ingest

import (
	"context"
	"errors"
	"testing"

	"mediacheck/internal/model"
)

func mustGenerate(t *testing.T, cfg Config) *Generated {
	t.Helper()
	g, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return g
}

// collect drains a reader and returns packets and faults separately.
func collect(t *testing.T, g *Generated, limits ReaderLimits) ([]model.Packet, []model.Fault, Stats) {
	t.Helper()
	r, err := NewReader(g.Fixture(), limits)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	var packets []model.Packet
	var faults []model.Fault
	err = r.Run(context.Background(), func(ev Event) error {
		if ev.Packet != nil {
			packets = append(packets, *ev.Packet)
		}
		if ev.Fault != nil {
			faults = append(faults, *ev.Fault)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return packets, faults, r.Stats()
}

func TestReaderCleanStreamHasNoFaults(t *testing.T) {
	cfg := DefaultConfig(17)
	cfg.VideoFrames = 100
	g := mustGenerate(t, cfg)
	packets, faults, stats := collect(t, g, DefaultReaderLimits())
	if len(faults) != 0 {
		t.Fatalf("clean run produced %d faults: %+v", len(faults), faults)
	}
	if len(packets) != g.AudioTotal+g.VideoTotal {
		t.Fatalf("emitted %d packets, want %d", len(packets), g.AudioTotal+g.VideoTotal)
	}
	if stats.Duplicates != 0 || stats.Lost != 0 {
		t.Fatalf("clean stats = %+v", stats)
	}
}

func TestReaderDamagedProfileReportsLossAndDuplicates(t *testing.T) {
	cfg := DefaultConfig(17)
	cfg.VideoFrames = 200
	g := mustGenerate(t, cfg)
	inj, err := g.ApplyProfile(ProfileDamaged)
	if err != nil {
		t.Fatal(err)
	}
	if len(inj.Dropped) == 0 || len(inj.Duplicated) == 0 {
		t.Fatalf("profile injected nothing: %+v", inj)
	}
	_, faults, stats := collect(t, g, DefaultReaderLimits())
	var lost, dup, regress int
	for _, f := range faults {
		switch f.Kind {
		case model.FaultLoss:
			lost++
		case model.FaultDuplicate:
			dup++
		case model.FaultTimestampRegres:
			regress++
		}
	}
	if lost != len(inj.Dropped) {
		t.Fatalf("loss faults = %d, want %d", lost, len(inj.Dropped))
	}
	if dup != len(inj.Duplicated) {
		t.Fatalf("duplicate faults = %d, want %d", dup, len(inj.Duplicated))
	}
	if regress != len(inj.Regressed) {
		t.Fatalf("regression faults = %d, want %d", regress, len(inj.Regressed))
	}
	if stats.Duplicates != len(inj.Duplicated) {
		t.Fatalf("stats duplicates = %d, want %d", stats.Duplicates, len(inj.Duplicated))
	}
	// Duplicates must not be counted twice in the carried packet count.
	wantPackets := len(g.Audio.Packets) + len(g.Video.Packets) - len(inj.Duplicated)
	if stats.Emitted != wantPackets {
		t.Fatalf("emitted %d packets, want %d (duplicates excluded)", stats.Emitted, wantPackets)
	}
}

func TestReaderWrapProfileDetectsWrap(t *testing.T) {
	cfg := DefaultConfig(17)
	cfg.VideoFrames = 200
	g := mustGenerate(t, cfg)
	inj, err := g.ApplyProfile(ProfileWrap)
	if err != nil {
		t.Fatal(err)
	}
	_, faults, stats := collect(t, g, DefaultReaderLimits())
	var wraps, lost int
	for _, f := range faults {
		switch f.Kind {
		case model.FaultSequenceWrap:
			wraps++
		case model.FaultLoss:
			lost++
		}
	}
	if wraps != 1 {
		t.Fatalf("wrap faults = %d, want 1", wraps)
	}
	if lost != 1 {
		t.Fatalf("loss faults = %d, want 1 merged gap of %d", lost, len(inj.Dropped))
	}
	if stats.Lost != len(inj.Dropped) {
		t.Fatalf("stats lost = %d, want %d", stats.Lost, len(inj.Dropped))
	}
	if stats.Wraps != 1 {
		t.Fatalf("stats wraps = %d, want 1", stats.Wraps)
	}
}

func TestReaderTruncatedProfileKeepsLaterPackets(t *testing.T) {
	cfg := DefaultConfig(17)
	cfg.VideoFrames = 200
	g := mustGenerate(t, cfg)
	inj, err := g.ApplyProfile(ProfileTruncated)
	if err != nil {
		t.Fatal(err)
	}
	packets, faults, stats := collect(t, g, DefaultReaderLimits())
	if stats.Emitted != g.AudioTotal+g.VideoTotal {
		t.Fatalf("truncation hid %d packets", g.AudioTotal+g.VideoTotal-stats.Emitted)
	}
	if len(packets) != g.AudioTotal+g.VideoTotal {
		t.Fatalf("emitted %d, want %d", len(packets), g.AudioTotal+g.VideoTotal)
	}
	// No sequence faults: truncation must not be confused with loss.
	for _, f := range faults {
		if f.Kind == model.FaultLoss || f.Kind == model.FaultDuplicate {
			t.Fatalf("truncation produced sequence fault %+v", f)
		}
	}
	if len(inj.Truncated) == 0 {
		t.Fatal("profile truncated nothing")
	}
}

func TestReaderRejectsLimitViolations(t *testing.T) {
	cfg := DefaultConfig(17)
	cfg.VideoFrames = 50
	g := mustGenerate(t, cfg)
	r, err := NewReader(g.Fixture(), ReaderLimits{MaxPackets: 10, MaxBytes: 1 << 20, MaxSpan: 0})
	if err != nil {
		t.Fatal(err)
	}
	err = r.Run(context.Background(), func(Event) error { return nil })
	if err == nil {
		t.Fatal("expected limit error")
	}
	if !errors.Is(err, model.ErrLimitExceeded) {
		t.Fatalf("error = %v, want ErrLimitExceeded", err)
	}
}

func TestReaderRejectsOversizedPacket(t *testing.T) {
	cfg := DefaultConfig(17)
	cfg.VideoFrames = 10
	g := mustGenerate(t, cfg)
	g.Audio.Packets[3].Data = make([]byte, MaxPacketBytes+1)
	r, err := NewReader(g.Fixture(), DefaultReaderLimits())
	if err != nil {
		t.Fatal(err)
	}
	err = r.Run(context.Background(), func(Event) error { return nil })
	if !errors.Is(err, model.ErrLimitExceeded) {
		t.Fatalf("error = %v, want ErrLimitExceeded", err)
	}
}

func TestReaderHonoursCancellation(t *testing.T) {
	cfg := DefaultConfig(17)
	cfg.VideoFrames = 100
	g := mustGenerate(t, cfg)
	r, err := NewReader(g.Fixture(), DefaultReaderLimits())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	count := 0
	err = r.Run(ctx, func(Event) error {
		count++
		if count == 5 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, model.ErrCanceled) {
		t.Fatalf("error = %v, want ErrCanceled", err)
	}
	if count != 5 {
		t.Fatalf("read %d events after cancel, want 5", count)
	}
}

func TestParseProfile(t *testing.T) {
	for _, p := range AllProfiles() {
		got, err := ParseProfile(string(p))
		if err != nil || got != p {
			t.Fatalf("ParseProfile(%q) = %v, %v", p, got, err)
		}
	}
	if _, err := ParseProfile("chaos"); err == nil {
		t.Fatal("expected error for unknown profile")
	}
}
