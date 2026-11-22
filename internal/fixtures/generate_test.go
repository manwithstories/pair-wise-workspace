package fixtures

import (
	"errors"
	"testing"

	"github.com/clearliu/fieldcut/internal/clock"
	"github.com/clearliu/fieldcut/internal/ingest"
)

func shortCfg(seed int64) Config {
	c := DefaultConfig(seed)
	c.DurationTicks = clock.Ticks(2 * clock.Rate) // 50 frames at 40ms
	c.LossRate = 0
	return c
}

func TestGenerateDeterministic(t *testing.T) {
	cfg := shortCfg(41)
	a, err := GeneratePackets(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GeneratePackets(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Seq != b[i].Seq || a[i].TS != b[i].TS || string(a[i].Raw) != string(b[i].Raw) {
			t.Fatalf("non-deterministic packet at %d", i)
		}
	}
	// Different seed must change the stream once randomness is active.
	other := shortCfg(42)
	other.LossRate = 0.3
	c, _ := GeneratePackets(other)
	same := true
	for i := 0; i < len(a) && i < len(c); i++ {
		if string(a[i].Raw) != string(c[i].Raw) || a[i].IsRTX != c[i].IsRTX {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different seeds produced identical streams")
	}
}

func TestGeneratedPacketsParse(t *testing.T) {
	cfg := shortCfg(7)
	pkts, err := GeneratePackets(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pkts {
		parsed, err := ingest.ParsePacket(p.Raw)
		if err != nil {
			t.Fatalf("generated packet fails validation: %v", err)
		}
		if parsed.SequenceNumber != p.Seq || parsed.Timestamp != p.TS {
			t.Fatalf("round trip mismatch: %d/%d vs %d/%d",
				parsed.SequenceNumber, parsed.Timestamp, p.Seq, p.TS)
		}
	}
}

func TestLossInjection(t *testing.T) {
	cfg := shortCfg(11)
	cfg.LossRate = 0.3
	pkts, err := GeneratePackets(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var rtx int
	for _, p := range pkts {
		if p.IsRTX {
			rtx++
		}
	}
	if rtx == 0 {
		t.Fatal("loss rate 0.3 produced no RTX recoveries")
	}
	// Sequence numbers must repeat at least once (a loss followed by RTX).
	seen := map[uint16]int{}
	for _, p := range pkts {
		seen[p.Seq]++
	}
	var repeated bool
	for _, n := range seen {
		if n > 1 {
			repeated = true
		}
	}
	if !repeated {
		t.Fatal("expected at least one sequence to appear twice (RTX)")
	}
}

func TestDuplicateAndLateInjection(t *testing.T) {
	cfg := shortCfg(13)
	cfg.DuplicateRate = 0.5
	pkts, err := GeneratePackets(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var dup bool
	for _, p := range pkts {
		if p.IsDuplicate {
			dup = true
		}
	}
	if !dup {
		t.Fatal("duplicate rate produced no duplicates")
	}

	cfg = shortCfg(13)
	cfg.LateRate = 1
	pkts, err = GeneratePackets(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var late bool
	for _, p := range pkts {
		if p.IsLate {
			late = true
		}
	}
	if !late {
		t.Fatal("late injection produced no late packet")
	}
}

func TestTimestampWrapGeneration(t *testing.T) {
	cfg := shortCfg(17)
	cfg.Wrap = true
	pkts, err := GeneratePackets(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// With wrap enabled a raw timestamp discontinuity must appear.
	var wrapSeen bool
	for i := 1; i < len(pkts); i++ {
		if pkts[i].TS < pkts[i-1].TS && !pkts[i].IsLate && !pkts[i].IsRTX {
			wrapSeen = true
		}
	}
	if !wrapSeen {
		t.Fatal("wrap fixture produced no raw timestamp discontinuity")
	}
}

func TestGeneratedSegmentsValidate(t *testing.T) {
	cfg := shortCfg(19)
	segs, err := GenerateSegments(cfg, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range segs {
		rep, err := ingest.ValidateSegment(s.Data)
		if err != nil {
			t.Fatalf("segment %d failed validation: %v", s.Sequence, err)
		}
		if rep.Kind != ingest.KindMediaSegment || rep.Fragments != 1 {
			t.Fatalf("segment %d report wrong: %+v", s.Sequence, rep)
		}
	}
}

func TestTruncatedSegmentIsolated(t *testing.T) {
	cfg := shortCfg(23)
	cfg.TruncateBox = true
	segs, err := GenerateSegments(cfg, 0)
	if err != nil {
		t.Fatal(err)
	}
	var truncated int
	for _, s := range segs {
		_, err := ingest.ValidateSegment(s.Data)
		switch {
		case s.Truncated:
			truncated++
			if !errors.Is(err, clock.ErrTruncatedBox) {
				t.Fatalf("truncated segment %d: want ErrTruncatedBox, got %v", s.Sequence, err)
			}
		case err != nil:
			t.Fatalf("healthy segment %d rejected: %v", s.Sequence, err)
		}
	}
	if truncated != 1 {
		t.Fatalf("want exactly 1 truncated segment, got %d", truncated)
	}
}

func TestHLSTemplate(t *testing.T) {
	raw := HLSTemplate([]int{100, 101, 102}, 4)
	if string(raw[:7]) != "#EXTM3U" {
		t.Fatal("playlist missing header")
	}
	if got := string(raw); !contains(got, "#EXT-X-MEDIA-SEQUENCE:100") ||
		!contains(got, "seg100.m4s") || !contains(got, "seg102.m4s") {
		t.Fatalf("playlist content wrong:\n%s", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestRejectBadConfig(t *testing.T) {
	cfg := shortCfg(1)
	cfg.LossRate = 1.5
	if _, err := GeneratePackets(cfg); !errors.Is(err, clock.ErrInvalidArgument) {
		t.Fatalf("bad loss rate: %v", err)
	}
	cfg = shortCfg(1)
	cfg.DurationTicks = 0
	if _, err := GeneratePackets(cfg); !errors.Is(err, clock.ErrInvalidArgument) {
		t.Fatalf("bad duration: %v", err)
	}
}
