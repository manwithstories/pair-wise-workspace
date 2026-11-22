package verify

import (
	"os"
	"path/filepath"
	"testing"

	"medialine/internal/fault"
	"medialine/internal/manifest"
	"medialine/internal/mp4"
	"medialine/internal/model"
	"medialine/internal/testkit"
)

// buildFixture writes a one-segment manifest plus its segment file.
func buildFixture(t *testing.T) (dir string, entry manifest.Entry) {
	t.Helper()
	dir = t.TempDir()
	clip, err := testkit.GenerateClip(t.TempDir(), testkit.ClipSpec{
		Name: "src", Seed: 3, Tracks: []testkit.TrackSpec{testkit.DefaultVideoTrack(1, 1_000_000, 10)},
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := mp4.Parse(clip.Path, mp4.Options{})
	if err != nil {
		t.Fatal(err)
	}
	tr := f.Tracks[0]
	sr, err := f.OpenSampleReader(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()
	plan := mp4.SegmentPlan{ID: "v720/t1/seg", TrackID: 1, Timescale: tr.Timescale, Codec: "vide", StartUS: 0, EndUS: 500_000}
	for _, s := range tr.Samples {
		if s.PTSUS >= 500_000 {
			break
		}
		b, err := sr.ReadSample(s)
		if err != nil {
			t.Fatal(err)
		}
		plan.Samples = append(plan.Samples, s)
		plan.Payloads = append(plan.Payloads, append([]byte(nil), b...))
	}
	segPath := filepath.Join(dir, "v720", "seg.m4s")
	if _, err := mp4.WriteSegment(segPath, plan, mp4.WriterOptions{Root: dir}); err != nil {
		t.Fatal(err)
	}
	hash, err := HashFile(segPath)
	if err != nil {
		t.Fatal(err)
	}
	nums := make([]int, 0, len(plan.Samples))
	for _, s := range plan.Samples {
		nums = append(nums, s.Number)
	}
	entry = manifest.Entry{
		ID:            plan.ID,
		Representation: "v720",
		TrackID:       1,
		Kind:          model.SegmentSimple,
		StartUS:       0,
		EndUS:         500_000,
		SampleNumbers: nums,
		File:          "v720/seg.m4s",
		ContentHash:   hash,
		Status:        "complete",
	}
	m := &manifest.Manifest{Version: manifest.ManifestVersion, SegmentUS: 500_000, SpanUS: 500_000, Entries: []manifest.Entry{entry}}
	if _, err := manifest.Write(m, dir); err != nil {
		t.Fatal(err)
	}
	return dir, entry
}

func TestRunVerifiesCompleteFragments(t *testing.T) {
	dir, _ := buildFixture(t)
	rep, err := Run(dir, mp4.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total != 1 || rep.Completed != 1 || rep.Verified != 1 || rep.Failed != 0 {
		t.Fatalf("report=%+v", rep)
	}
	if rep.MatchRate() != 1 {
		t.Fatalf("match rate=%v", rep.MatchRate())
	}
	if !rep.Results[0].RoundTrip {
		t.Fatal("expected round-trip success")
	}
}

func TestRunDetectsTampering(t *testing.T) {
	dir, _ := buildFixture(t)
	path := filepath.Join(dir, "v720", "seg.m4s")
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xff}, 12); err != nil {
		t.Fatal(err)
	}
	f.Close()

	rep, err := Run(dir, mp4.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verified != 0 || rep.Failed != 1 {
		t.Fatalf("report=%+v", rep)
	}
	fe := rep.Results[0].Err
	if fe == nil || fe.Kind != fault.KindCorruptSample {
		t.Fatalf("err=%v", fe)
	}
}

func TestRunSkipsIncompleteEntries(t *testing.T) {
	dir, entry := buildFixture(t)
	entry.Status = "failed"
	entry.File = "v720/missing.m4s"
	m := &manifest.Manifest{Version: manifest.ManifestVersion, SegmentUS: 1, SpanUS: 1, Entries: []manifest.Entry{entry}}
	if _, err := manifest.Write(m, dir); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(dir, mp4.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 1 || rep.Completed != 0 || rep.Failed != 0 {
		t.Fatalf("report=%+v", rep)
	}
	if rep.MatchRate() != 1 {
		t.Fatalf("rate=%v", rep.MatchRate())
	}
}

func TestRunReportsMissingFileAndHash(t *testing.T) {
	dir, entry := buildFixture(t)
	entry.ID = "other"
	entry.File = "v720/nope.m4s"
	entry.ContentHash = ""
	m := &manifest.Manifest{Version: manifest.ManifestVersion, SegmentUS: 1, SpanUS: 1, Entries: []manifest.Entry{entry}}
	if _, err := manifest.Write(m, dir); err != nil {
		t.Fatal(err)
	}
	rep, _ := Run(dir, mp4.Options{})
	if rep.Failed != 1 || rep.Results[0].Err == nil || rep.Results[0].Err.Kind != fault.KindMissingIndex {
		t.Fatalf("report=%+v err=%v", rep, rep.Results[0].Err)
	}
}

func TestRunRejectsDuplicateIDs(t *testing.T) {
	dir, entry := buildFixture(t)
	m := &manifest.Manifest{Version: manifest.ManifestVersion, SegmentUS: 1, SpanUS: 1, Entries: []manifest.Entry{entry, entry}}
	if _, err := manifest.Write(m, dir); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(dir, mp4.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Issues) != 1 {
		t.Fatalf("issues=%v", rep.Issues)
	}
}

func TestRunMissingManifest(t *testing.T) {
	if _, err := Run(t.TempDir(), mp4.Options{}); err == nil {
		t.Fatal("expected manifest error")
	}
}

func TestHashSegments(t *testing.T) {
	dir, _ := buildFixture(t)
	hashes, missing, err := HashSegments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 1 || len(missing) != 0 {
		t.Fatalf("hashes=%v missing=%v", hashes, missing)
	}
	os.Remove(filepath.Join(dir, "v720", "seg.m4s"))
	_, missing, _ = HashSegments(dir)
	if len(missing) != 1 {
		t.Fatalf("missing=%v", missing)
	}
}

func TestRelUnderRoot(t *testing.T) {
	root := t.TempDir()
	if !RelUnderRoot(root, "a/b.m4s") {
		t.Fatal("nested path should be under root")
	}
	if RelUnderRoot(root, "../x") {
		t.Fatal("escaping path accepted")
	}
}

func TestHashFileMissing(t *testing.T) {
	if _, err := HashFile(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error")
	}
}
