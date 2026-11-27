package report

import (
	"errors"
	"strings"
	"testing"

	"archive/internal/model"
)

func baseCounters() Counters {
	return Counters{
		TargetKbps: 1800, ActualKbps: 1782, PacketsTotal: 100, CRCMatched: 100,
		SegmentsTotal: 4, SegmentsComplete: 4, Retransmitted: 3, FECRecovered: 2,
		Gaps: 0, PeakBytes: 4096, MemoryLimit: 8388608,
		SyncErrorNanos: int64(12 * model.Millisecond), Backpressure: 1,
	}
}

func TestBuildHealthySummary(t *testing.T) {
	s, err := Build("normal-41", baseCounters())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if s.Status != StatusOK || s.Gaps != 0 {
		t.Fatalf("healthy run must be ok, got %s gaps=%d", s.Status, s.Gaps)
	}
	if s.DeltaPct != -1.0 {
		t.Fatalf("delta = %.2f, want -1.00", s.DeltaPct)
	}
	if s.QualityCRCPct != 100.0 || s.Recovered != 5 {
		t.Fatalf("crc/recovered wrong: %+v", s)
	}
	if s.SyncErrorMs != 12.0 {
		t.Fatalf("sync ms = %.2f, want 12", s.SyncErrorMs)
	}
	if len(s.SemanticSHA) != 64 {
		t.Fatalf("semantic digest must be 32 bytes hex, got len %d", len(s.SemanticSHA))
	}
	out := s.Render()
	for _, key := range []string{"case", "target_kbps", "actual_kbps", "delta_pct",
		"quality_crc_pct", "sync_error_ms", "recovered", "gaps", "peak_bytes",
		"semantic_sha256"} {
		if !strings.Contains(out, key+"=") {
			t.Fatalf("rendered output missing contract key %q:\n%s", key, out)
		}
	}
}

func TestBuildDegradedWhenGaps(t *testing.T) {
	c := baseCounters()
	c.Gaps = 2
	c.SegmentsComplete = 3
	s, err := Build("damaged-41", c)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if s.Status != StatusDegraded {
		t.Fatalf("gaps must yield degraded, got %s", s.Status)
	}
}

func TestBuildRejectsInvariantViolations(t *testing.T) {
	mem := baseCounters()
	mem.PeakBytes = mem.MemoryLimit + 1
	if _, err := Build("x", mem); !errors.Is(err, model.ErrInvariant) {
		t.Fatalf("peak over limit must be invariant error, got %v", err)
	}
	bad := baseCounters()
	bad.CRCMatched = bad.PacketsTotal + 1
	if _, err := Build("x", bad); !errors.Is(err, model.ErrInvariant) {
		t.Fatalf("crc over total must be invariant error, got %v", err)
	}
	badSeg := baseCounters()
	badSeg.SegmentsComplete = badSeg.SegmentsTotal + 1
	if _, err := Build("x", badSeg); !errors.Is(err, model.ErrInvariant) {
		t.Fatalf("complete over total must be invariant error, got %v", err)
	}
}

func TestSemanticDigestDeterministicAndSensitive(t *testing.T) {
	s1, err := Build("normal-41", baseCounters())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s2, _ := Build("normal-41", baseCounters())
	if s1.SemanticSHA != s2.SemanticSHA {
		t.Fatal("identical counters must produce identical semantic digest")
	}
	changed := baseCounters()
	changed.FECRecovered++ // a different recovery outcome must change the digest
	s3, _ := Build("normal-41", changed)
	if s3.SemanticSHA == s1.SemanticSHA {
		t.Fatal("semantic digest did not react to a changed outcome")
	}
}

func TestRenderTableSorted(t *testing.T) {
	c := baseCounters()
	sB, _ := Build("b-case", c)
	sA, _ := Build("a-case", c)
	out := RenderTable([]Summary{sB, sA})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if !strings.HasPrefix(lines[1], "a-case") || !strings.HasPrefix(lines[2], "b-case") {
		t.Fatalf("table rows not sorted by case:\n%s", out)
	}
}
