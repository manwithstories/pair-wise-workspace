package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"mediabridge/internal/fault"
	"mediabridge/internal/fixture"
)

// TestUnknownAdapterVersionRejected checks that a manifest pinning an
// unregistered adapter version is rejected as an input error (exit code 2)
// with a located fault, rather than silently parsed with a newer adapter.
func TestUnknownAdapterVersionRejected(t *testing.T) {
	dir := t.TempDir()
	rep, err := fixture.GenerateMP4(dir, "clip.mp4", fixture.DefaultMP4Options(5))
	if err != nil {
		t.Fatal(err)
	}
	_ = rep
	manifest := `{
	  "version": 1,
	  "root_dir": ".",
	  "assets": [{
	    "id": "BADVER",
	    "primary": {"format": "mp4", "path": "clip.mp4", "version": 99},
	    "mapping": {"drop_unmapped": true}
	  }]
	}`
	mpath := filepath.Join(dir, "m.json")
	if err := os.WriteFile(mpath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	results, runErr := Run(mpath, Options{OutDir: filepath.Join(dir, "out")})
	if runErr.Fatal != nil {
		t.Fatal(runErr.Fatal)
	}
	if len(results) != 1 || results[0].Status != "rejected" {
		t.Fatalf("results = %+v", results)
	}
	if got := fault.CodeOf(results[0].Error); got != fault.CodeUnknownVersion {
		t.Fatalf("code = %s", got)
	}
	if fault.ExitCode(results[0].Error) != 2 {
		t.Fatalf("exit code = %d", fault.ExitCode(results[0].Error))
	}
	if !results[0].Error.Loc.HasTrack && results[0].Error.Loc.SourcePath == "" {
		t.Fatal("unknown-version error lacks location")
	}
}

// TestMappingFailureIsolated verifies that an asset whose mapping cannot be
// satisfied is rejected while a sibling asset still completes.
func TestMappingFailureIsolated(t *testing.T) {
	dir := t.TempDir()
	if _, err := fixture.GenerateMP4(dir, "a.mp4", fixture.DefaultMP4Options(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.GenerateMP4(dir, "b.mp4", fixture.DefaultMP4Options(2)); err != nil {
		t.Fatal(err)
	}
	manifest := `{
	  "version": 1,
	  "root_dir": ".",
	  "assets": [
	    {"id": "OK", "primary": {"path": "a.mp4"},
	     "mapping": {"drop_unmapped": true}},
	    {"id": "NOV", "primary": {"path": "b.mp4"},
	     "mapping": {"video_track_id": 999, "drop_unmapped": true}}
	  ]
	}`
	mpath := filepath.Join(dir, "m.json")
	os.WriteFile(mpath, []byte(manifest), 0o644)
	results, runErr := Run(mpath, Options{OutDir: filepath.Join(dir, "out")})
	if runErr.Fatal != nil {
		t.Fatal(runErr.Fatal)
	}
	status := map[string]string{}
	for _, r := range results {
		status[r.AssetID] = r.Status
	}
	if status["OK"] != "copied" {
		for _, r := range results {
			if r.AssetID == "OK" {
				t.Fatalf("OK asset status = %s err=%v failed=%v", r.Status, r.Error, r.Failed)
			}
		}
	}
	if status["NOV"] != "rejected" {
		t.Fatalf("NOV asset status = %s", status["NOV"])
	}
}

// TestCheckpointQuarantinedOnSourceChange ensures a re-mastered source
// invalidates the old checkpoint and reprocesses from scratch.
func TestCheckpointQuarantinedOnSourceChange(t *testing.T) {
	dir := t.TempDir()
	demo, err := GenerateDemo(DemoOptions{Seed: 3, RootDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if _, rerr := Run(demo.ManifestPath, Options{OutDir: out, InterruptAfter: 50}); rerr.Fatal != nil {
		t.Fatal(rerr.Fatal)
	}
	// Regenerate PT-001's source with a different seed, changing its digest.
	changed := fixture.DefaultMP4Options(999)
	if _, err := fixture.GenerateMP4(filepath.Join(dir, "media"), "archive_pt1.mp4", changed); err != nil {
		t.Fatal(err)
	}
	results, rerr := Run(demo.ManifestPath, Options{OutDir: out})
	if rerr.Fatal != nil {
		t.Fatal(rerr.Fatal)
	}
	for _, r := range results {
		if r.AssetID == "PT-001" {
			if r.Quarantined == 0 {
				t.Fatal("expected PT-001 checkpoint quarantine after source change")
			}
			if r.Status == "rejected" {
				t.Fatalf("PT-001 rejected: %v", r.Error)
			}
			return
		}
	}
	t.Fatal("PT-001 missing")
}
