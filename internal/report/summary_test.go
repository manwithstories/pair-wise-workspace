package report

import (
	"strings"
	"testing"

	"mediascan/internal/clock"
	"mediascan/internal/fault"
	"mediascan/internal/pipeline"
)

func sampleResult() *pipeline.Result {
	return &pipeline.Result{
		Params:            pipeline.Params{Rate: clock.Rate2997DF},
		Segments:          []pipeline.SegmentReport{{Index: 0, Source: "a.mp4", Kind: pipeline.KindMP4, Bytes: 100, KeyframeGaps: 2, DamageOffset: 50}},
		Fingerprints:      map[string]string{"a.mp4": "deadbeef"},
		RecoveredSegments: 1,
		KeyframeGapTotal:  2,
		Faults: []fault.Fault{
			fault.New("a.mp4", 50, fault.StageMP4, true, "truncated box"),
			fault.New("a.rtp", -1, fault.StageRTP, true, "network gap"),
		},
	}
}

func TestBuildDeterministic(t *testing.T) {
	s1 := Build(sampleResult())
	s2 := Build(sampleResult())
	if s1.SHA256 != s2.SHA256 || s1.Text != s2.Text {
		t.Fatal("identical results must produce identical summary and hash")
	}
	if len(s1.SHA256) != 64 {
		t.Fatalf("sha256 length = %d, want 64 hex", len(s1.SHA256))
	}
}

func TestChecksumSensitiveToContent(t *testing.T) {
	base := Build(sampleResult())
	mod := sampleResult()
	mod.RecoveredSegments = 2
	if Build(mod).SHA256 == base.SHA256 {
		t.Fatal("changed result must change checksum")
	}
}

func TestKeyLinesContract(t *testing.T) {
	lines := KeyLines(sampleResult(), 123, 50)
	joined := strings.Join(lines, "\n")
	for _, key := range []string{"segments=", "keyframe_gaps=", "damage_offset=",
		"recovered_segments=", "resume_ms=", "sha256="} {
		if !strings.Contains(joined, key) {
			t.Fatalf("key line %q missing from:\n%s", key, joined)
		}
	}
	if !strings.Contains(joined, "sha256=") {
		t.Fatal("sha256 line required")
	}
	// resume_ms must not perturb the checksum.
	a := KeyLines(sampleResult(), 1, 50)
	b := KeyLines(sampleResult(), 999, 50)
	if a[len(a)-1] != b[len(b)-1] {
		t.Fatal("resume timing must not change the hashed checksum line")
	}
}

func TestFirstDamageOffsetIgnoresNetworkFaults(t *testing.T) {
	if got := FirstDamageOffset(sampleResult().Faults); got != 50 {
		t.Fatalf("first damage offset = %d, want 50 (network offset -1 ignored)", got)
	}
	none := []fault.Fault{fault.New("a.rtp", -1, fault.StageRTP, true, "gap")}
	if got := FirstDamageOffset(none); got != -1 {
		t.Fatalf("network-only faults must yield -1, got %d", got)
	}
}
