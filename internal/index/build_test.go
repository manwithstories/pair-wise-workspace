package index

import (
	"strings"
	"testing"

	"bcast/internal/boxes"
	"bcast/internal/clock"
	"bcast/internal/fixture"
)

func repairedFixture(t *testing.T, caseName string) (string, *clock.Result) {
	t.Helper()
	dir := t.TempDir()
	if _, err := fixture.Generate(caseName, dir); err != nil {
		t.Fatal(err)
	}
	scan, err := boxes.ScanBatch(dir, boxes.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	anchors, err := clock.LoadAnchors(dir)
	if err != nil {
		t.Fatal(err)
	}
	tl, err := clock.Repair(scan, anchors, clock.RepairOptions{AllowAnchorlessClamp: true})
	if err != nil {
		t.Fatal(err)
	}
	return dir, tl
}

func TestBuildAndVerifySeekAndManifest(t *testing.T) {
	dir, tl := repairedFixture(t, "drift-wrap-vfr")
	art, err := Build(tl)
	if err != nil {
		t.Fatal(err)
	}
	if len(art.Entries) != 24 {
		t.Fatalf("entries=%d want 24", len(art.Entries))
	}
	if string(art.SIDX[4:8]) != "sidx" {
		t.Fatalf("sidx box type missing: % x", art.SIDX[:min(12, len(art.SIDX))])
	}
	if string(art.TFRA[4:8]) != "tfra" {
		t.Fatalf("tfra box type missing: % x", art.TFRA[:8])
	}
	if !strings.Contains(string(art.Manifest), "#EXTM3U") || !strings.Contains(string(art.Manifest), "seg-00001.m4s") {
		t.Fatalf("manifest missing expected content:\n%s", art.Manifest)
	}
	res, err := Verify(dir, art)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.SeekOK != 24 || res.SeekTotal != 24 {
		t.Fatalf("seek=%d/%d want 24/24", res.SeekOK, res.SeekTotal)
	}
	if res.ManifestSkew > 2 {
		t.Fatalf("skew=%.3f ms > 2", res.ManifestSkew)
	}
}

func TestVerifyRejectsBadSeekPointAndSkew(t *testing.T) {
	dir, tl := repairedFixture(t, "commit-interrupt")
	art, err := Build(tl)
	if err != nil {
		t.Fatal(err)
	}
	good := art.Entries[0].MoofOffset
	art.Entries[0].MoofOffset = 1
	if _, err := Verify(dir, art); err == nil {
		t.Fatal("expected bad seek offset to fail")
	}
	art.Entries[0].MoofOffset = good

	original := string(art.Manifest)
	art.Manifest = []byte(strings.Replace(original, "#EXTINF:0.800000", "#EXTINF:0.810000", 1))
	if strings.Contains(string(art.Manifest), "#EXTINF:0.810000") {
		_, err := Verify(dir, art)
		if err == nil {
			t.Fatal("expected 10 ms manifest skew to fail")
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
