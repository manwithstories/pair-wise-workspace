package report_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"avcheck/internal/media"
	"avcheck/internal/report"
	"avcheck/internal/timeline"
)

func sampleResult() *timeline.Result {
	faults := &media.Faults{}
	faults.Addf(media.LevelRecovered, media.CodePacketRecovered, 1, 4, "recovered sequence 4")
	return &timeline.Result{
		Anchor: 2,
		Tracks: []timeline.TrackTimeline{
			{Track: 1, Kind: media.KindVideo, FirstNs: 0, LastNs: 8e9,
				Samples:   make([]timeline.Sample, 80),
				Gaps:      1,
				Recovered: 2, Monotonic: true},
			{Track: 2, Kind: media.KindAudio, FirstNs: 0, LastNs: 8e9,
				Samples: make([]timeline.Sample, 400), Monotonic: true},
			{Track: 3, Kind: media.KindCue, FirstNs: 25e6, LastNs: 8e9,
				Samples: make([]timeline.Sample, 40), Monotonic: true},
		},
		CueOrder:           []uint64{2, 1, 3},
		AVSkewP95Ns:        12e6,
		AVSkewMaxNs:        19e6,
		ManifestDeltaNs:    0,
		PayloadBytes:       900_000,
		PeakBufferBytes:    640_000,
		RecoveredPackets:   2,
		UnrecoveredPackets: 1,
		Duplicates:         3,
		RecoveredPct:       66.6,
		Faults:             faults,
	}
}

func TestBuildMetrics(t *testing.T) {
	m, err := report.Build(sampleResult(), report.Options{
		TargetBps: 900_000, DurationNs: 8e9, Seed: 731, MemoryCap: 8 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.ActualBps != 900_000 {
		t.Fatalf("actual bps = %d, want 900000", m.ActualBps)
	}
	if m.DeltaPct != 0 {
		t.Fatalf("delta pct = %.4f, want 0", m.DeltaPct)
	}
	if m.AVSkewP95Ms != 12 {
		t.Fatalf("p95 ms = %.3f, want 12", m.AVSkewP95Ms)
	}
	if m.RecoveredPackets != 2 {
		t.Fatalf("recovered = %d, want 2", m.RecoveredPackets)
	}
	if got := m.Quality.VideoGapPct; got != 1.25 {
		t.Fatalf("video gap pct = %.3f, want 1.25", got)
	}
	if len(m.Faults) != 1 || m.Faults[0].Code != string(media.CodePacketRecovered) {
		t.Fatalf("faults = %+v", m.Faults)
	}
	if m.PeakBufferBytes > 8<<20 {
		t.Fatalf("peak buffer %d exceeds cap", m.PeakBufferBytes)
	}
}

// TestDigestIsStableAndExcludesItself requires the hash to be independent of
// when it is computed and absent from its own input.
func TestDigestIsStableAndExcludesItself(t *testing.T) {
	m, err := report.Build(sampleResult(), report.Options{TargetBps: 900_000, DurationNs: 8e9, Seed: 731})
	if err != nil {
		t.Fatal(err)
	}
	d1, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}
	m.SHA256 = "stale"
	d2, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("digest depends on prior hash: %s vs %s", d1, d2)
	}
	if len(d1) != 64 {
		t.Fatalf("digest length = %d, want 64", len(d1))
	}
}

// TestBuildIsDeterministic requires equal inputs to yield byte-identical
// manifests, which the acceptance criteria demand per seed.
func TestBuildIsDeterministic(t *testing.T) {
	opt := report.Options{TargetBps: 900_000, DurationNs: 8e9, Seed: 731}
	a, _ := report.Build(sampleResult(), opt)
	b, _ := report.Build(sampleResult(), opt)
	ja, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	jb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ja, jb) {
		t.Fatal("manifests differ for identical input")
	}
}

func TestWriteAtomicLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	m, err := report.Build(sampleResult(), report.Options{TargetBps: 900_000, DurationNs: 8e9, Seed: 731})
	if err != nil {
		t.Fatal(err)
	}
	path, err := report.WriteAtomic(dir, m)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "manifest.json" {
		t.Fatalf("path = %s", path)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "manifest.json" {
			t.Fatalf("leftover file %q", e.Name())
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(m.SHA256)) {
		t.Fatal("manifest does not contain its own digest")
	}
}

func TestDiagnosticsMentionsFaultAndOffset(t *testing.T) {
	faults := &media.Faults{}
	faults.Addf(media.LevelAbort, media.CodeTruncatedVint, 0, 261, "cut vint")
	body := report.Diagnostics(faults)
	if !bytes.Contains([]byte(body), []byte("truncated_vint")) ||
		!bytes.Contains([]byte(body), []byte("offset=261")) {
		t.Fatalf("diagnostics = %q", body)
	}
	if !bytes.Contains([]byte(body), []byte("not_committed")) {
		t.Fatalf("diagnostics missing status: %q", body)
	}
}

func TestSummaryLineHasEveryMetric(t *testing.T) {
	m, err := report.Build(sampleResult(), report.Options{TargetBps: 900_000, DurationNs: 8e9})
	if err != nil {
		t.Fatal(err)
	}
	m.SHA256 = "deadbeef"
	line := m.SummaryLine()
	for _, want := range []string{
		"target_bps=", "actual_bps=", "delta_pct=", "recovered_packets=",
		"quality.video_gap_pct=", "quality.audio_gap_ms=", "av_skew_p95_ms=",
		"manifest_delta_ms=", "peak_buffer_bytes=", "sha256=",
	} {
		if !bytes.Contains([]byte(line), []byte(want)) {
			t.Fatalf("summary line missing %q: %s", want, line)
		}
	}
}
