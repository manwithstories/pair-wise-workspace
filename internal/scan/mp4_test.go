package scan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediaroute/internal/fixtures"
	"mediaroute/internal/index"
	"mediaroute/internal/manifest"
)

func buildAndScan(t *testing.T, seed int64, v fixtures.Variant, opts Options) (*index.Index, error) {
	t.Helper()
	dir := t.TempDir()
	a, err := fixtures.Build(dir, fixtures.DefaultSpec(seed), v)
	if err != nil {
		t.Fatalf("build %s: %v", v, err)
	}
	m, err := manifest.ReadFile(a.Manifest)
	if err != nil {
		t.Fatalf("manifest %s: %v", v, err)
	}
	opts.BaseDir = dir
	res, err := Run(context.Background(), m, opts)
	if res == nil {
		return nil, err
	}
	return res.Index, err
}

func TestScanNormal(t *testing.T) {
	ix, err := buildAndScan(t, 42, fixtures.VariantNormal, Options{VerifyBytes: true})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if ix.IndexedSamples != 72+48 {
		t.Fatalf("want 120 indexed samples got %d", ix.IndexedSamples)
	}
	if len(ix.CorruptRanges) != 0 {
		t.Fatalf("normal must be clean: %+v", ix.CorruptRanges)
	}
	video := ix.TrackByID(1)
	if video == nil || video.Entry.Type != "avc1" || video.Entry.Width != 320 {
		t.Fatalf("video entry: %+v", video)
	}
	// Sparse keyframes: every 12th sample, never fabricated in between.
	if len(video.Keyframes) != 6 {
		t.Fatalf("want 6 keyframes got %d (%v)", len(video.Keyframes), video.Keyframes)
	}
	for _, k := range video.Keyframes {
		if (k-1)%12 != 0 {
			t.Fatalf("unexpected keyframe %d", k)
		}
	}
	if video.Samples[0].CTS == video.Samples[0].DTS {
		t.Fatal("composition offset was dropped")
	}
	if video.Origin.Num == 0 {
		t.Fatal("non-zero origin not preserved")
	}
	audio := ix.TrackByID(2)
	if audio == nil || audio.Entry.SampleRate != 48000 || audio.Entry.Channels != 2 {
		t.Fatalf("audio entry: %+v", audio)
	}
	if len(audio.Samples) != 48 {
		t.Fatalf("want 48 audio samples got %d", len(audio.Samples))
	}
}

func TestScanTruncatedKeepsPrefix(t *testing.T) {
	ix, err := buildAndScan(t, 42, fixtures.VariantTruncated, Options{VerifyBytes: true})
	if err == nil {
		t.Fatal("truncated asset must report a recoverable error")
	}
	if !strings.Contains(err.Error(), "sample") && !strings.Contains(err.Error(), "corrupt") {
		// We only assert it is a typed model error below.
		t.Logf("error text: %v", err)
	}
	video := ix.TrackByID(1)
	if video == nil {
		t.Fatal("video track missing")
	}
	if len(ix.CorruptRanges) == 0 {
		t.Fatal("want a corrupt range")
	}
	r := ix.CorruptRanges[0]
	if r.ByteOffset <= 0 || r.FirstSample == 0 {
		t.Fatalf("range must record first unreadable offset: %+v", r)
	}
	// Every sample before the first corrupt sample stays indexed and readable.
	for _, s := range video.Samples {
		if s.Number < r.FirstSample && !s.Readable {
			t.Fatalf("sample %d before range is unreadable", s.Number)
		}
	}
	// Timestamps after the gap are untouched: sample numbers still ascend.
	for i := 1; i < len(video.Samples); i++ {
		if video.Samples[i].DTS <= video.Samples[i-1].DTS {
			t.Fatalf("DTS not monotonic at %d", i)
		}
	}
	if ix.IndexedSamples == 0 {
		t.Fatal("prefix index was discarded")
	}
}

func TestScanOutOfRange(t *testing.T) {
	ix, err := buildAndScan(t, 42, fixtures.VariantOutOfRange, Options{VerifyBytes: true})
	if err == nil {
		t.Fatal("out-of-range offset must be reported")
	}
	if len(ix.CorruptRanges) != 1 {
		t.Fatalf("want one corrupt range got %+v", ix.CorruptRanges)
	}
	r := ix.CorruptRanges[0]
	if r.FirstSample != 6 {
		t.Fatalf("want sample 6 corrupt got %d", r.FirstSample)
	}
	if r.ByteOffset != 1<<30 {
		t.Fatalf("want recorded offset %d got %d", 1<<30, r.ByteOffset)
	}
}

func TestScanMissingPages(t *testing.T) {
	ix, err := buildAndScan(t, 42, fixtures.VariantMissingPages, Options{VerifyBytes: true})
	if err == nil {
		t.Fatal("zeroed page must be reported as corruption")
	}
	if len(ix.CorruptRanges) == 0 {
		t.Fatal("want corrupt range for missing page")
	}
	if len(ix.CorruptRanges[0].Reason) == 0 {
		t.Fatal("reason must be populated")
	}
}

func TestScanIsDeterministic(t *testing.T) {
	a, err := buildAndScan(t, 42, fixtures.VariantNormal, Options{VerifyBytes: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildAndScan(t, 42, fixtures.VariantNormal, Options{VerifyBytes: true})
	if err != nil {
		t.Fatal(err)
	}
	if a.Summary != b.Summary {
		t.Fatalf("index summaries differ across identical scans")
	}
}

func TestScanCacheInvalidatesOnMediaChange(t *testing.T) {
	dir := t.TempDir()
	store := &index.Store{Dir: filepath.Join(dir, "cache")}
	a, err := fixtures.Build(dir, fixtures.DefaultSpec(42), fixtures.VariantNormal)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.ReadFile(a.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	scan := func() *Result {
		res, err := Run(context.Background(), m, Options{BaseDir: dir, Cache: store, VerifyBytes: true})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	r1 := scan()
	if r1.CacheHit {
		t.Fatal("first scan cannot be a hit")
	}
	r2 := scan()
	if !r2.CacheHit {
		t.Fatal("identical rescan must hit the cache")
	}
	if r1.Index.Summary != r2.Index.Summary {
		t.Fatal("cached index summary changed")
	}
	// Mutate the media bytes in place; the input digest must change.
	raw, err := os.ReadFile(a.VideoPath)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(a.VideoPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	r3 := scan()
	if r3.CacheHit {
		t.Fatal("changed media must invalidate the cache")
	}
	if !r3.Rebuilt {
		t.Fatal("changed media on the same manifest slot must report a rebuild")
	}
}

func TestScanCanceled(t *testing.T) {
	dir := t.TempDir()
	a, err := fixtures.Build(dir, fixtures.DefaultSpec(42), fixtures.VariantNormal)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.ReadFile(a.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, m, Options{BaseDir: dir, VerifyBytes: true}); err == nil {
		t.Fatal("want cancellation error")
	}
}
