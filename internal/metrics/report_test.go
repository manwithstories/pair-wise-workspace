// Package metrics: unit tests for the independent verification maths.
package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"archive/internal/cache"
	"archive/internal/checkpoint"
	"archive/internal/demux"
	"archive/internal/fixture"
	"archive/internal/pipeline"
)

// seedArchive builds a small committed archive through the real
// pipeline, so the metrics tests check it against genuine output.
func seedArchive(t *testing.T, root string) ([]checkpoint.Entry, error) {
	t.Helper()
	if _, err := fixture.Generate(root, fixture.Options{
		Segments: 2, TruncateAt: -1, MissingAt: -1, DuplicateAt: -1, ConflictAt: -1,
	}); err != nil {
		return nil, err
	}
	res, err := pipeline.Run(pipeline.Config{Root: root, Manifest: "media.m3u8", InterruptAfter: -1})
	if err != nil {
		return nil, err
	}
	cp, err := checkpoint.Open(filepath.Join(root, "work", "checkpoints"))
	if err != nil {
		return nil, err
	}
	entries, err := cp.Entries()
	if err != nil {
		return nil, err
	}
	if len(entries) != len(res.Entries) {
		t.Fatalf("checkpoint entries %d != run entries %d", len(entries), len(res.Entries))
	}
	return entries, nil
}

func track(handler string, ts uint32, pts ...int64) demux.Track {
	t := demux.Track{Handler: handler, Timescale: ts, Lang: "und", Codec: "x"}
	for _, p := range pts {
		t.Samples = append(t.Samples, demux.Sample{DTS: uint64(p), Dur: ts / 25})
	}
	return t
}

func TestP95NearestRank(t *testing.T) {
	var xs []time.Duration
	for i := 1; i <= 100; i++ {
		xs = append(xs, time.Duration(i)*time.Millisecond)
	}
	if got := p95(xs); got != 95 {
		t.Fatalf("p95 = %v, want 95", got)
	}
	if got := p95([]time.Duration{time.Millisecond}); got != 1 {
		t.Fatalf("single-sample p95 = %v", got)
	}
	if got := p95(nil); got != 0 {
		t.Fatalf("empty p95 = %v", got)
	}
}

func TestMeasureSyncFromTimestamps(t *testing.T) {
	// Video starts at 1s (90000 ticks); audio at 1.010s (48480 ticks).
	v := track("vide", 90000, 90000)
	a := track("soun", 48000, 48480)
	ms, table, err := measureSync([]demux.Track{v, a})
	if err != nil {
		t.Fatal(err)
	}
	if ms < 9.9 || ms > 10.1 {
		t.Fatalf("sync = %v ms, want ~10", ms)
	}
	if len(table) != 2 {
		t.Fatalf("track table = %v", table)
	}
}

func TestMeasureSyncSingleTrack(t *testing.T) {
	ms, _, err := measureSync([]demux.Track{track("vide", 90000, 0)})
	if err != nil || ms != 0 {
		t.Fatalf("single track sync = %v, %v", ms, err)
	}
}

func TestMeasureSyncRejectsEmptyTrack(t *testing.T) {
	empty := demux.Track{Handler: "soun", Timescale: 48000}
	if _, _, err := measureSync([]demux.Track{empty}); err == nil {
		t.Fatal("a track without samples must be rejected")
	}
}

func TestVerifyChainRejectsSizeDrift(t *testing.T) {
	root := t.TempDir()
	entries, err := seedArchive(t, root)
	if err != nil {
		t.Fatal(err)
	}
	cas, err := cache.Open(filepath.Join(root, "work", "cas"))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyChain(cas, entries); err != nil {
		t.Fatalf("fresh archive should verify: %v", err)
	}
	bad := append([]checkpoint.Entry(nil), entries...)
	bad[0].OutputBytes++
	if err := verifyChain(cas, bad); err == nil {
		t.Fatal("size drift must be detected")
	}
}

func TestBuildMeasuresEverySegment(t *testing.T) {
	root := t.TempDir()
	entries, err := seedArchive(t, root)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Build(root, []time.Duration{time.Millisecond, 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Segments != len(entries) || rep.CacheVerified != len(entries) {
		t.Fatalf("report = %+v", rep)
	}
	if rep.SyncErrorMs > SyncToleranceMs {
		t.Fatalf("sync error %v exceeds tolerance; desync=%v", rep.SyncErrorMs, rep.Desync)
	}
	if len(rep.OutputSHA) != 64 || len(rep.TrackTable) != 2 {
		t.Fatalf("bad report: sha=%q table=%v", rep.OutputSHA, rep.TrackTable)
	}
	if len(rep.Provenance) != len(entries) {
		t.Fatalf("provenance lines = %d, want %d", len(rep.Provenance), len(entries))
	}
	for _, line := range rep.Provenance {
		if !strings.Contains(line, "<-sha256:") || !strings.Contains(line, "tracks=2") {
			t.Fatalf("provenance line %q lacks source detail", line)
		}
	}
}

func TestProvenanceDigestMismatchDetected(t *testing.T) {
	root := t.TempDir()
	entries, err := seedArchive(t, root)
	if err != nil {
		t.Fatal(err)
	}
	e := entries[0]
	path := filepath.Join(root, "work", "provenance", "seg-000000000.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), e.OutputSHA, strings.Repeat("0", 64), 1)
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.ReadProvenance(root, e); err == nil {
		t.Fatal("provenance/checkpoint digest disagreement must be rejected")
	}
}

func TestMissingProvenanceIsFatal(t *testing.T) {
	root := t.TempDir()
	if _, err := seedArchive(t, root); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "work", "provenance", "seg-000000000.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(root, nil); err == nil {
		t.Fatal("missing provenance for a committed segment must fail verification")
	}
}
