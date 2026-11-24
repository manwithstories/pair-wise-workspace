package demo

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mediaroute/internal/fixtures"
	"mediaroute/internal/index"
	"mediaroute/internal/manifest"
	"mediaroute/internal/scan"
)

func TestRunAcceptanceThresholds(t *testing.T) {
	res, err := Run(context.Background(), Options{
		Seed:          42,
		Repeat:        1,
		LossBurst:     12,
		TruncateTail:  4096,
		ReorderWindow: 64,
		Quiet:         true,
	})
	if err != nil {
		t.Fatalf("demo: %v", err)
	}
	if res.ExitCode != 0 {
		rep := res.Takes[0].Report
		t.Fatalf("acceptance exit %d: %+v", res.ExitCode, rep)
	}
	rep := res.Takes[0].Report
	if rep.SyncErrorMSP95 > 20 {
		t.Fatalf("sync p95 %f exceeds 20", rep.SyncErrorMSP95)
	}
	if rep.PacketRecoveryRate < 0.95 {
		t.Fatalf("recovery rate %f below 0.95", rep.PacketRecoveryRate)
	}
	if rep.LatencyMSP95 > 250 {
		t.Fatalf("latency p95 %f exceeds 250", rep.LatencyMSP95)
	}
	if rep.PacketsLost == 0 {
		t.Fatal("loss burst should be reflected in packet loss")
	}
	if rep.IndexedSamples == 0 {
		t.Fatal("no indexed samples")
	}
	if rep.CorruptRanges != 0 {
		t.Fatalf("normal asset reports %d corrupt ranges", rep.CorruptRanges)
	}
	if rep.LayoutReason == "" {
		t.Fatal("layout decision must be recorded")
	}
	if rep.OutputLUFS < -17 || rep.OutputLUFS > -15 {
		t.Fatalf("output loudness %f off target", rep.OutputLUFS)
	}
	if rep.TruePeakDBTP > -0.99 {
		t.Fatalf("true peak %f exceeds ceiling", rep.TruePeakDBTP)
	}
}

func TestRunRepeatIsStableAndCaches(t *testing.T) {
	res, err := Run(context.Background(), Options{Seed: 42, Repeat: 2, ReorderWindow: 64, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stable {
		t.Fatal("repeat digests must be identical")
	}
	second := res.Takes[1].Report
	if !second.CacheHit {
		t.Fatal("second run must hit the index cache")
	}
	if second.IndexRebuilt {
		t.Fatal("second run must not rebuild")
	}
	if res.Takes[0].Artifact != res.Takes[1].Artifact {
		t.Fatal("artifact digests differ")
	}
}

func TestMutatedMediaRebuildsIndex(t *testing.T) {
	dir := t.TempDir()
	first, err := Run(context.Background(), Options{WorkDir: dir, Seed: 42, ReorderWindow: 64, Quiet: true, KeepWorkDir: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = first
	path := filepath.Join(dir, "normal-video.mp4")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := Run(context.Background(), Options{WorkDir: dir, Seed: 42, ReorderWindow: 64, Quiet: true, KeepWorkDir: true, ReuseExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	rep := second.Takes[0].Report
	if !rep.IndexRebuilt {
		t.Fatal("mutated media must force an index rebuild")
	}
	if rep.CacheHit {
		t.Fatal("mutated media must not be a cache hit")
	}
}

func TestFaultVariantsBoundaries(t *testing.T) {
	res, err := Run(context.Background(), Options{Seed: 42, ReorderWindow: 64, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Faults) != 3 {
		t.Fatalf("want 3 fault checks got %d", len(res.Faults))
	}
	for _, f := range res.Faults {
		if !f.Recoverable {
			t.Fatalf("%s must be recoverable", f.Variant)
		}
		if f.CorruptRanges == 0 {
			t.Fatalf("%s reported no corrupt range", f.Variant)
		}
		if !f.StreamBoundaryOK {
			t.Fatalf("%s scan and stream boundaries disagree", f.Variant)
		}
		if f.IsolatedSamples == 0 || f.IndexedSamples == 0 {
			t.Fatalf("%s must keep prefix samples and isolate the rest: %+v", f.Variant, f)
		}
	}
}

func TestInvalidConfiguration(t *testing.T) {
	cases := []Options{
		{Seed: -1, ReorderWindow: 64},
		{Seed: 1, ReorderWindow: 0},
		{Seed: 1, ReorderWindow: 64, TruncateTail: -5},
		{Seed: 1, ReorderWindow: 64, LossBurst: -1},
	}
	for _, c := range cases {
		if _, err := Run(context.Background(), c); err == nil {
			t.Fatalf("config %+v must be rejected", c)
		}
	}
}

func TestScanOnlyAndVerify(t *testing.T) {
	dir := t.TempDir()
	a, err := fixtures.Build(dir, fixtures.DefaultSpec(42), fixtures.VariantNormal)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ScanOnly(context.Background(), ScanOptions{Manifest: a.Manifest, BaseDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.Takes[0].Report.IndexedSamples == 0 {
		t.Fatal("scan-only found no samples")
	}
	ok, _, err := Verify(context.Background(), a.Manifest, filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("verify must confirm a stable, cached index")
	}
}

func TestIndexView(t *testing.T) {
	ix := &index.Index{IndexedSamples: 7, Summary: "abc"}
	ix.CorruptRanges = make([]index.CorruptRange, 2)
	v := indexView{ix}
	if v.Samples() != 7 || v.CorruptCount() != 2 || v.Summary() != "abc" {
		t.Fatalf("bad view %d %d %s", v.Samples(), v.CorruptCount(), v.Summary())
	}
}

func TestManifestErrorsAreNotInternal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"tracks":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanOnly(context.Background(), ScanOptions{Manifest: path}); err == nil {
		t.Fatal("empty manifest must fail")
	}
	if _, err := manifest.ReadFile(path); err == nil {
		t.Fatal("manifest reader must reject an empty track list")
	}
	_ = scan.Version
}
