package index_test

import (
	"os"
	"path/filepath"
	"testing"

	"mediascan/internal/core"
	"mediascan/internal/fixture"
	"mediascan/internal/index"
)

func buildFixture(t *testing.T, fault fixture.Fault) (string, *index.Index) {
	t.Helper()
	root := t.TempDir()
	if err := fixture.Generate(root, fault); err != nil {
		t.Fatalf("generate: %v", err)
	}
	idx, err := index.Build(root, index.Config{Hashes: true})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return root, idx
}

func TestBuildClean(t *testing.T) {
	_, idx := buildFixture(t, fixture.FaultNone)
	if idx.RoundTripOK != 3 || idx.RoundTripTotal != 3 {
		t.Fatalf("roundtrip = %d/%d, want 3/3", idx.RoundTripOK, idx.RoundTripTotal)
	}
	if len(idx.Segments) != fixture.TotalSegments {
		t.Fatalf("segments = %d, want %d", len(idx.Segments), fixture.TotalSegments)
	}
	if len(idx.TimelineErrors) != 0 {
		t.Fatalf("timeline errors = %v, want none", idx.TimelineErrors)
	}
	// HLS discontinuity sequences: 0,0,0,1,1,1,1,2,2,2.
	wantDisc := map[string]int{
		"hls/seg0.m4s": 0, "hls/seg2.m4s": 0,
		"hls/seg3.m4s": 1, "hls/seg6.m4s": 1,
		"hls/seg7.m4s": 2, "hls/seg9.m4s": 2,
	}
	// Unified timeline: HLS covers 0..5s in 0.5s steps; both DASH reps
	// cover 0..2.5s.
	wantTime := map[string][2]core.Rat{
		"hls/seg0.m4s":   {core.NewRat(0, 1), core.NewRat(1, 2)},
		"hls/seg9.m4s":   {core.NewRat(9, 2), core.NewRat(5, 1)},
		"dash/a/seg4.m4s": {core.NewRat(2, 1), core.NewRat(5, 2)},
		"dash/b/seg0.m4s": {core.NewRat(0, 1), core.NewRat(1, 2)},
	}
	for _, s := range idx.Segments {
		if d, ok := wantDisc[s.ID]; ok && s.DiscSeq != d {
			t.Fatalf("%s disc seq = %d, want %d", s.ID, s.DiscSeq, d)
		}
		if w, ok := wantTime[s.ID]; ok && (s.Start != w[0] || s.End != w[1]) {
			t.Fatalf("%s time = %s..%s, want %s..%s", s.ID, s.Start, s.End, w[0], w[1])
		}
	}
}

func TestTfdtRollbackLandsInIndex(t *testing.T) {
	_, idx := buildFixture(t, fixture.FaultTfdtRollback)
	if len(idx.TimelineErrors) == 0 {
		t.Fatal("tfdt rollback must produce a timeline error range")
	}
	// The error must cover dash/a/seg2's interval [1, 3/2).
	target := core.NewRat(5, 4) // inside [1, 3/2)
	found := false
	for _, r := range idx.TimelineErrors {
		if r.Source != "timeline" {
			t.Fatalf("timeline error source = %q", r.Source)
		}
		if r.Contains(target) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no timeline error covers %s: %v", target, idx.TimelineErrors)
	}
}

func TestDiscontinuityBoundaryConflict(t *testing.T) {
	_, idx := buildFixture(t, fixture.FaultDiscBoundary)
	if len(idx.TimelineErrors) == 0 {
		t.Fatal("PDT rollback across discontinuity must produce a timeline error")
	}
	// seg7 starts at 7/2 on the unified timeline.
	target := core.NewRat(7, 2)
	found := false
	for _, r := range idx.TimelineErrors {
		if r.Contains(target) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no timeline error covers %s: %v", target, idx.TimelineErrors)
	}
}

func TestInvalidDurationFailsBuild(t *testing.T) {
	root := t.TempDir()
	if err := fixture.Generate(root, fixture.FaultInvalidDuration); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := index.Build(root, index.Config{}); err == nil {
		t.Fatal("invalid manifest duration must fail the index build")
	}
}

func TestStoreInvalidation(t *testing.T) {
	root := t.TempDir()
	if err := fixture.Generate(root, fixture.FaultNone); err != nil {
		t.Fatalf("generate: %v", err)
	}
	state := t.TempDir()
	cfg := index.Config{Hashes: true}

	_, rebuilt, err := index.LoadOrBuild(state, root, cfg)
	if err != nil || !rebuilt {
		t.Fatalf("first load: rebuilt=%v err=%v, want rebuilt", rebuilt, err)
	}
	_, rebuilt, err = index.LoadOrBuild(state, root, cfg)
	if err != nil || rebuilt {
		t.Fatalf("second load: rebuilt=%v err=%v, want cached", rebuilt, err)
	}
	// Config change invalidates.
	_, rebuilt, err = index.LoadOrBuild(state, root, index.Config{Hashes: false})
	if err != nil || !rebuilt {
		t.Fatalf("config change: rebuilt=%v err=%v, want rebuilt", rebuilt, err)
	}
	// Input change invalidates.
	seg := filepath.Join(root, "hls", "seg0.m4s")
	data, err := os.ReadFile(seg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seg, append(data, 0), 0o644); err != nil {
		t.Fatal(err)
	}
	_, rebuilt, err = index.LoadOrBuild(state, root, index.Config{Hashes: false})
	if err != nil || !rebuilt {
		t.Fatalf("input change: rebuilt=%v err=%v, want rebuilt", rebuilt, err)
	}
}

func TestSafeJoinRejectsEscape(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"../x", "a/../../b", "/abs/path", "http://evil/x.m3u8", "//host/share"} {
		if _, err := index.SafeJoin(root, bad); err == nil {
			t.Fatalf("SafeJoin(%q) should fail", bad)
		}
	}
	p, err := index.SafeJoin(root, "hls/seg0.m4s")
	if err != nil {
		t.Fatalf("SafeJoin valid: %v", err)
	}
	if p != filepath.Join(root, "hls/seg0.m4s") {
		t.Fatalf("SafeJoin = %q", p)
	}
}
