package pipeline

import (
	"math"
	"path/filepath"
	"testing"

	"mediad/internal/av"
	"mediad/internal/fixtures"
	"mediad/internal/manifest"
	"mediad/internal/media"
)

func TestMatrixCaseRenders(t *testing.T) {
	stats, err := runCase(t, fixtures.CaseMatrix, 41)
	if err != nil {
		t.Fatalf("matrix: %v", err)
	}
	if stats.IntegratedLUFS < -17 || stats.IntegratedLUFS > -15 {
		t.Fatalf("lufs=%.2f", stats.IntegratedLUFS)
	}
	if stats.ClippedSamples != 0 {
		t.Fatalf("clipped=%d", stats.ClippedSamples)
	}
	if stats.BitrateErrorPct > 8 || stats.BitrateErrorPct < -8 {
		t.Fatalf("bitrate err=%.2f", stats.BitrateErrorPct)
	}
}

func TestBadLayoutRejected(t *testing.T) {
	root := t.TempDir()
	set, err := fixtures.Build(fixtures.Spec{Root: filepath.Join(root, "fx"), Seed: 41, Case: fixtures.CaseBadLayout})
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.ParseManifestFile(set.Manifest)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	e, err := manifest.ParseEditsFile(set.Edits)
	if err != nil {
		t.Fatalf("edits: %v", err)
	}
	_, err = Run(Options{MediaRoot: set.Root, Manifest: m, Edits: e, OutDir: filepath.Join(root, "out")})
	if err == nil {
		t.Fatal("unknown layout accepted")
	}
	me, ok := media.AsMediaError(err)
	if !ok || me.Code != media.ErrUnsupported {
		t.Fatalf("code=%v err=%v", me, err)
	}
	if media.ExitCode(err) != 2 {
		t.Fatalf("exit=%d", media.ExitCode(err))
	}
}

// TestMonoCentredDuplicatesChannel checks that a mono source appears on both
// delivery channels with equal level.
func TestMonoCentredDuplicatesChannel(t *testing.T) {
	root := t.TempDir()
	set, err := fixtures.Build(fixtures.Spec{Root: root, Seed: 41, Case: fixtures.CaseMatrix})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := manifest.ParseManifestFile(set.Manifest)
	entry, _ := m.Lookup("camMono", "a1")
	p, err := manifest.ResolveWithin(set.Root, entry.File)
	if err != nil {
		t.Fatal(err)
	}
	res, err := av.ReadSegment(av.SegmentRequest{
		Path: p, StreamIndex: *entry.StreamIndex, TimeBase: entry.TimeBase(),
		InRaw: 48000, OutRaw: 96000, InLayout: entry.Layout, InChannels: entry.Channels,
	})
	if err != nil {
		t.Fatal(err)
	}
	var dl, dr float64
	for i := 0; i < res.LengthSamples; i++ {
		l := float64(res.Samples[i*2])
		r := float64(res.Samples[i*2+1])
		dl += l * l
		dr += r * r
	}
	if math.Abs(dl-dr) > 1e-3*(dl+dr) {
		t.Fatalf("mono not centred equally: L=%.4f R=%.4f", dl, dr)
	}
	if dl == 0 {
		t.Fatal("silent mono render")
	}
}
