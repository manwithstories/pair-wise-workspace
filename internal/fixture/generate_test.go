package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"mediadelivery/internal/manifest"
)

func TestGenerateCleanAndBoundary(t *testing.T) {
	for _, profile := range []Profile{ProfileClean, ProfileBoundary} {
		dir := t.TempDir()
		ds, err := Generate(Options{
			WorkDir: dir, Format: "both", Cameras: 3,
			DurationSec: 45, Profile: profile,
		})
		if err != nil {
			t.Fatalf("generate %s: %v", profile, err)
		}
		if len(ds.Cameras) != 3 {
			t.Fatalf("cameras = %d", len(ds.Cameras))
		}
		plan := ds.Plan
		if profile == ProfileBoundary {
			if len(plan.LostSlotSeqs) != 10 {
				t.Fatalf("lost slots = %d, want 10", len(plan.LostSlotSeqs))
			}
			if len(plan.UnrecoverableSeqs) != 1 {
				t.Fatalf("unrecoverable = %d, want 1", len(plan.UnrecoverableSeqs))
			}
			if len(plan.FaultsFor("", "truncated"))+countKind(plan, "truncated") < 1 {
				t.Fatal("expected truncated fault")
			}
		}

		// Every camera must have parseable DASH and an HLS master.
		for _, layout := range ds.Cameras {
			if _, err := manifest.ParseDASH(layout.Camera, layout.DASHMPD); err != nil {
				t.Fatalf("dash parse %s: %v", layout.Camera, err)
			}
			// Backup HLS variant must always parse (no faults injected there).
			backupPlaylist := filepath.Join(layout.Root, "backup", "index.m3u8")
			rep, err := manifest.ParseHLS(layout.Camera, backupPlaylist)
			if err != nil {
				t.Fatalf("backup hls parse %s: %v", layout.Camera, err)
			}
			if len(rep.Representations[0].Segments) == 0 {
				t.Fatalf("no backup segments for %s", layout.Camera)
			}
			// Segment files must exist for a clean run.
			if profile == ProfileClean {
				for _, s := range rep.Representations[0].Segments {
					p := filepath.Join(layout.Root, "backup", filepath.Base(s.URI))
					if _, err := os.Stat(p); err != nil {
						t.Fatalf("missing clean segment %s: %v", p, err)
					}
				}
			}
		}
	}
}

func countKind(p *Plan, kind string) int {
	n := 0
	for _, f := range p.Faults {
		if f.Kind == kind {
			n++
		}
	}
	return n
}

func TestBuildPlanBoundaryCounts(t *testing.T) {
	plan := BuildPlan(ProfileBoundary, CameraNames(3), 23)
	if got := countKind(plan, "lost"); got != 10 {
		t.Fatalf("lost = %d, want 10", got)
	}
	if got := countKind(plan, "truncated"); got != 1 {
		t.Fatalf("truncated = %d, want 1", got)
	}
	if got := countKind(plan, "out_of_order"); got != 1 {
		t.Fatalf("out_of_order = %d, want 1", got)
	}
	if got := countKind(plan, "pts_reset"); got != 1 {
		t.Fatalf("pts_reset = %d, want 1", got)
	}
	if got := countKind(plan, "color_conflict"); got != 1 {
		t.Fatalf("color_conflict = %d, want 1", got)
	}
	recoverable := 0
	for _, f := range plan.Faults {
		if f.Kind == "lost" && f.Recoverable {
			recoverable++
		}
	}
	if recoverable != 9 {
		t.Fatalf("recoverable lost = %d, want 9", recoverable)
	}
}
