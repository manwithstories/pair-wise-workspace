package report

import (
	"encoding/json"
	"strings"
	"testing"

	"mediatime/internal/clock"
	"mediatime/internal/segment"
)

func sampleTimeline() *clock.Timeline {
	return &clock.Timeline{
		Samples: []clock.Sample{
			{Sequence: 0, TrackID: 1, SampleNumber: 1, DTS: 0, PTS: 900, IsSync: true},
			{Sequence: 0, TrackID: 1, SampleNumber: 2, DTS: 45_000, PTS: 45_900},
			{Sequence: 1, TrackID: 1, SampleNumber: 1, DTS: 90_000, PTS: 90_900, IsSync: true},
			{Sequence: 1, TrackID: 1, SampleNumber: 2, DTS: 135_000, PTS: 135_900, IsUndecidable: true},
		},
		WrapCount: 1,
		Drift:     clock.DriftModel{DriftPPM: 187.345, Slope: 1.000187345},
		MinDTS:    0,
		MaxDTS:    135_000,
		Undecidable: 1,
	}
}

func TestComputeBitrateAndQuality(t *testing.T) {
	tl := sampleTimeline()
	idx := &segment.Index{Keys: 2, SHA256: strings.Repeat("a", 64)}
	in := Inputs{
		ManifestFragments: 2,
		TargetBPS:         800_000,
		// 时长 = 135000 Tick = 1.5s；字节 150000 → 8*150000/1.5 = 800000 bps。
		MediaBytes: 150_000,
		Timeline:   tl,
		Index:      idx,
		ManifestStarts: map[int]clock.Tick{
			0: 0,
			1: 90_000,
		},
		Thresholds: Thresholds{BitrateErrorPct: 2, AlignmentMaxMs: 8},
	}
	m := Compute(in)
	if m.ActualBPS != 800_000 {
		t.Fatalf("actual bps = %d, want 800000", m.ActualBPS)
	}
	if m.BitrateErrorPct != 0 {
		t.Fatalf("bitrate error = %v", m.BitrateErrorPct)
	}
	// 1 个不可判定样本必须进入质量坏点，不能算成功。
	if m.QualityBad != 1 || m.Completed != 2 {
		t.Fatalf("quality/completed wrong: bad=%d completed=%d", m.QualityBad, m.Completed)
	}
	if m.IndexKeys != 2 || m.IndexSHA256 != strings.Repeat("a", 64) {
		t.Fatal("index fields not propagated")
	}
	if m.WrapCount != 1 || m.DriftPPM != 187.35 {
		t.Fatalf("wrap/drift wrong: %+v", m)
	}
	if m.Status != StatusOK {
		t.Fatalf("status = %s, want ok", m.Status)
	}
}

func TestAlignmentErrorCaptured(t *testing.T) {
	tl := sampleTimeline()
	// 清单标称片段 1 起点比修复值早 450 Tick（5ms）。
	in := Inputs{
		ManifestFragments: 2, TargetBPS: 800_000, MediaBytes: 1,
		Timeline: tl,
		ManifestStarts: map[int]clock.Tick{
			0: 0,
			1: 89_550,
		},
		Thresholds: Thresholds{BitrateErrorPct: 2, AlignmentMaxMs: 8},
	}
	m := Compute(in)
	if m.AlignmentMaxMs != 5.0 {
		t.Fatalf("alignment max = %v, want 5ms", m.AlignmentMaxMs)
	}
}

func TestStatusAndExitCodes(t *testing.T) {
	cases := []struct {
		status Status
		code   int
	}{
		{StatusOK, 0}, {StatusInvalid, 2}, {StatusPartial, 3},
		{StatusCanceled, 4}, {StatusInternal, 5},
	}
	for _, c := range cases {
		m := &Metrics{Status: c.status}
		if m.ExitCode() != c.code {
			t.Fatalf("%s -> %d, want %d", c.status, m.ExitCode(), c.code)
		}
	}
}

func TestPartialWhenCorruptedButProgress(t *testing.T) {
	tl := sampleTimeline()
	in := Inputs{
		ManifestFragments: 3, TargetBPS: 800_000, MediaBytes: 100,
		Timeline: tl, Corrupted: 1, ErrorCategory: "truncated_box",
		ManifestStarts: map[int]clock.Tick{0: 0, 1: 90_000},
		Thresholds:     Thresholds{BitrateErrorPct: 2, AlignmentMaxMs: 8},
	}
	m := Compute(in)
	if m.Status != StatusPartial || m.ExitCode() != 3 {
		t.Fatalf("want partial/3, got %s/%d", m.Status, m.ExitCode())
	}
}

func TestCanceledStatus(t *testing.T) {
	in := Inputs{
		ManifestFragments: 24, Cancelled: true, ErrorCategory: "cancelled",
		Thresholds: Thresholds{BitrateErrorPct: 2, AlignmentMaxMs: 8},
	}
	m := Compute(in)
	if m.Status != StatusCanceled || m.ExitCode() != 4 || !m.Cancelled {
		t.Fatalf("cancel classification wrong: %+v", m)
	}
}

func TestJSONAndSummaryDeterministic(t *testing.T) {
	m := Compute(Inputs{
		ManifestFragments: 1, TargetBPS: 800_000, MediaBytes: 100_000,
		Timeline:       sampleTimeline(),
		Index:          &segment.Index{Keys: 2, SHA256: strings.Repeat("f", 64)},
		ManifestStarts: map[int]clock.Tick{0: 0, 1: 90_000},
		Thresholds:     Thresholds{BitrateErrorPct: 2, AlignmentMaxMs: 8},
	})
	raw, err := m.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var back Metrics
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("json not machine-readable: %v", err)
	}
	if back.IndexSHA256 != m.IndexSHA256 {
		t.Fatal("json round trip mismatch")
	}
	s1, s2 := m.Summary(), m.Summary()
	if s1 != s2 || !strings.Contains(s1, "fragments=1") ||
		!strings.Contains(s1, "index_sha256="+strings.Repeat("f", 64)) {
		t.Fatalf("summary unstable or missing fields: %s", s1)
	}
}
