package remux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediac/internal/inspect"
	"mediac/internal/manifest"
)

// writeSegmentFiles materializes simple fragment payloads at the manifest's
// resolved absolute paths so Run has real files to copy.
func writeSegmentFiles(t *testing.T, m *manifest.Manifest) {
	t.Helper()
	for ri := range m.Representations {
		rep := &m.Representations[ri]
		for si := range rep.Segments {
			seg := &rep.Segments[si]
			payload := []byte("frag-bytes-" + itoa(seg.Index))
			if err := os.MkdirAll(filepath.Dir(seg.AbsPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(seg.AbsPath, payload, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func buildRemuxManifest(root string) *manifest.Manifest {
	m := &manifest.Manifest{Kind: manifest.KindDASH, Root: root}
	rep := manifest.Representation{
		ID: "v0", Codec: "avc1.640028", Width: 640, Height: 360,
		TargetBPS: 400000, Timescale: 1000,
	}
	for s := 0; s < 8; s++ {
		rel := "v0/seg-" + itoa(s+1) + ".m4s"
		abs := filepath.Join(root, filepath.FromSlash(rel))
		rep.Segments = append(rep.Segments, manifest.Segment{
			Index: s + 1, URL: rel, AbsPath: abs,
			StartMs: int64(s) * 1000, DurationMs: 1000, Line: s + 2,
		})
	}
	m.Representations = append(m.Representations, rep)
	return m
}

func cleanReports() map[int]*inspect.FragmentReport {
	out := map[int]*inspect.FragmentReport{}
	for f := 1; f <= 8; f++ {
		out[f] = &inspect.FragmentReport{RepID: "v0", Fragment: f, Timescale: 1000}
	}
	return out
}

func TestRemuxSkipsDamagedAndPreservesTimeline(t *testing.T) {
	root := t.TempDir()
	outDir := filepath.Join(t.TempDir(), "out")
	m := buildRemuxManifest(root)
	writeSegmentFiles(t, m)
	reports := cleanReports()
	// Damage segments 3 (truncated box) and 6 (transport incomplete).
	reports[3].Truncated = true
	complete := map[int]bool{}
	for f := 1; f <= 8; f++ {
		complete[f] = true
	}
	complete[6] = false

	out, err := Run(Plan{
		Root: root, OutDir: outDir, Manifest: m, RepIndex: 0,
		Reports: reports, Complete: complete,
		InitBytes: []byte("ftyp-moov-init"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.FragmentPaths) != 6 {
		t.Fatalf("kept fragments=%d want 6", len(out.FragmentPaths))
	}
	if len(out.SkippedSegments) != 2 ||
		out.SkippedSegments[0] != 3 || out.SkippedSegments[1] != 6 {
		t.Fatalf("skipped=%v want [3 6]", out.SkippedSegments)
	}
	// Original numbering retained: no seg-3 / seg-6 outputs.
	for _, skipped := range []int{3, 6} {
		bad := filepath.Join(outDir, "v0", "seg-"+itoa(skipped)+".m4s")
		if _, err := os.Stat(bad); !os.IsNotExist(err) {
			t.Fatalf("damaged fragment %d must not be remuxed", skipped)
		}
	}

	hls, err := os.ReadFile(out.ManifestHLS)
	if err != nil {
		t.Fatal(err)
	}
	hlsText := string(hls)
	if strings.Count(hlsText, "#EXT-X-DISCONTINUITY") != 2 {
		t.Fatalf("need 2 discontinuities for 2 gaps, got:\n%s", hlsText)
	}
	if strings.Contains(hlsText, "seg-3.m4s") || strings.Contains(hlsText, "seg-6.m4s") {
		t.Fatal("damaged segments must not appear in the HLS playlist")
	}
	dash, err := os.ReadFile(out.ManifestDASH)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(dash), `t="2000"`) || strings.Contains(string(dash), `t="5000"`) {
		t.Fatal("DASH timeline must omit damaged segment times, not renumber them")
	}
}

func TestRemuxDeterministicHashes(t *testing.T) {
	root := t.TempDir()
	m := buildRemuxManifest(root)
	writeSegmentFiles(t, m)
	plan := Plan{
		Root: root, Manifest: m, RepIndex: 0,
		Reports: cleanReports(), Complete: allComplete(8),
		InitBytes: []byte("init"),
	}
	out1, err := Run(withOut(plan, filepath.Join(t.TempDir(), "a")))
	if err != nil {
		t.Fatal(err)
	}
	out2, err := Run(withOut(plan, filepath.Join(t.TempDir(), "b")))
	if err != nil {
		t.Fatal(err)
	}
	if out1.Hashes.HLS != out2.Hashes.HLS || out1.Hashes.DASH != out2.Hashes.DASH {
		t.Fatal("manifest hashes must be identical across reruns")
	}
	if len(out1.Hashes.Fragments) != len(out2.Hashes.Fragments) {
		t.Fatal("fragment count differs across reruns")
	}
	for i := range out1.Hashes.Fragments {
		if out1.Hashes.Fragments[i] != out2.Hashes.Fragments[i] {
			t.Fatalf("fragment %d hash changed", i)
		}
	}
}

func TestRemuxRejectsNilReport(t *testing.T) {
	root := t.TempDir()
	m := buildRemuxManifest(root)
	writeSegmentFiles(t, m)
	out, err := Run(Plan{
		Root: root, OutDir: filepath.Join(t.TempDir(), "o"),
		Manifest: m, RepIndex: 0, Reports: map[int]*inspect.FragmentReport{},
		Complete: allComplete(8), InitBytes: []byte("init"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.FragmentPaths) != 0 || len(out.SkippedSegments) != 8 {
		t.Fatalf("missing reports must skip every fragment: %+v", out)
	}
}

func withOut(p Plan, out string) Plan {
	p.OutDir = out
	return p
}

func allComplete(n int) map[int]bool {
	m := map[int]bool{}
	for i := 1; i <= n; i++ {
		m[i] = true
	}
	return m
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
