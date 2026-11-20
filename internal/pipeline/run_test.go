package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediamap/internal/testkit"
)

func demoOpts() testkit.Options {
	return testkit.Options{Tracks: 4, Ladder: 3, DropBurst: 12, TruncateEvery: 7, Seed: 41}
}

func TestDemoMetricsInAcceptanceRanges(t *testing.T) {
	out, code := RunDemo(demoOpts())
	if code != 2 {
		t.Fatalf("recoverable damage must exit 2, got %d", code)
	}
	s := out.Summary
	check := func(name string, v, lo, hi float64) {
		t.Helper()
		if v < lo || v > hi {
			t.Errorf("%s=%.3f outside [%.1f, %.1f]", name, v, lo, hi)
		}
	}
	check("av_sync_ms", s.AVSyncMs, 2.0, 20.0)
	check("packet_recovery_pct", s.PacketRecoveryPct, 70.0, 99.0)
	check("p95_latency_ms", s.P95LatencyMs, 1.0, 500.0)
	check("alignment_error_ms", s.AlignmentErrorMs, 0.0, 80.0)
	if s.CorruptSegments == 0 {
		t.Error("corrupt_segments must be > 0")
	}
	if len(s.ManifestHash) != 64 {
		t.Errorf("manifest hash: %q", s.ManifestHash)
	}
	var located bool
	for _, f := range out.Faults {
		if f.Offset >= 0 {
			located = true
		}
	}
	if !located {
		t.Error("faults must carry source offsets")
	}
}

func TestDemoDeterministic(t *testing.T) {
	a, _ := RunDemo(demoOpts())
	b, _ := RunDemo(demoOpts())
	if a.Summary.ManifestHash != b.Summary.ManifestHash {
		t.Fatal("same seed must reproduce the same manifest hash")
	}
}

func TestCleanCaseExitsZero(t *testing.T) {
	// A fully indexed, undamaged two-track case: no faults, exit 0.
	build := func(name, kind string, timescale int64) testkit.TrackFixture {
		tf := testkit.TrackFixture{Name: name, Kind: kind, Timescale: timescale}
		var pl strings.Builder
		pl.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n")
		rep := testkit.RepFixture{ID: name + "-r0", Bandwidth: 400000}
		for i := 0; i < 5; i++ {
			uri := fmt.Sprintf("%s_seg_%d.m4s", rep.ID, i)
			data := testkit.MediaSegment(uint32(i+1), uint64(int64(i)*4*timescale), true, make([]byte, 300))
			rep.Segments = append(rep.Segments, testkit.SegmentFixture{URI: uri, Data: data})
			pl.WriteString("#EXTINF:4.000,\n" + uri + "\n")
		}
		pl.WriteString("#EXT-X-ENDLIST\n")
		rep.Playlist = []byte(pl.String())
		tf.Reps = []testkit.RepFixture{rep}
		return tf
	}
	c := testkit.Case{Tracks: []testkit.TrackFixture{
		build("v", "video", 90000),
		build("a", "audio", 48000),
	}}
	var faults int
	for _, tf := range c.Tracks {
		tr := processTrack(tf)
		faults += len(tr.Faults)
		if tr.Status != "ok" {
			t.Fatalf("%s: status %s", tf.Name, tr.Status)
		}
	}
	if faults != 0 {
		t.Fatalf("clean case must have no faults, got %d", faults)
	}
}

func TestFatalStructuralErrorTerminatesOnlyItsTrack(t *testing.T) {
	c := testkit.Generate(demoOpts())
	// Inject an unrecoverable structural error into one track.
	c.Tracks[0].Reps[0].Segments[0].Data = testkit.BadLength(c.Tracks[0].Reps[0].Segments[0].Data)
	tr := processTrack(c.Tracks[0])
	if tr.Status != "failed" {
		t.Fatalf("track with fatal error: %s", tr.Status)
	}
	other := processTrack(c.Tracks[1])
	if other.Status == "failed" {
		t.Fatal("fatal error must not leak across tracks")
	}
}

func TestVerifyFileRoundTrip(t *testing.T) {
	out, _ := RunDemo(demoOpts())
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "case.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	problems, err := VerifyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	out, _ := RunDemo(demoOpts())
	out.Tracks[0].Reps[0].Segments[0].URI = "tampered.m4s"
	data, _ := json.Marshal(out)
	path := filepath.Join(t.TempDir(), "case.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	problems, err := VerifyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) == 0 {
		t.Fatal("tampered manifest must fail verification")
	}
}

func TestGapsReportedForMissingCuts(t *testing.T) {
	out, _ := RunDemo(demoOpts())
	for _, tr := range out.Tracks {
		if len(tr.Alignment.Gaps) == 0 {
			t.Errorf("%s: corrupt moof segments must produce gaps", tr.Name)
		}
	}
}
