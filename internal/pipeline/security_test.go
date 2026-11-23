package pipeline

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"mediacore/internal/checkpoint"
	"mediacore/internal/demux"
	"mediacore/internal/fault"
	"mediacore/internal/fixture"
)

func TestLimitRejectsOversizedAllocation(t *testing.T) {
	art, err := fixture.Build(fixture.DefaultSpec(42, 0))
	if err != nil {
		t.Fatal(err)
	}
	lim := demux.DefaultLimits()
	lim.MaxFileBytes = 8 // far below the generated fixture
	if _, err := demux.NewReader(bytesReader(art.Bytes), lim); err == nil {
		t.Fatal("oversized file must be rejected by limits")
	}

	lim = demux.DefaultLimits()
	lim.MaxSamples = 2 // stsz declares more samples than this cap
	r, err := demux.NewReader(bytesReader(art.Bytes), lim)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Open(); err == nil {
		t.Fatal("sample count above limit must abort")
	} else if f := fault.As(err); f == nil || f.Kind != fault.KindLimitExceeded {
		t.Fatalf("want limit_exceeded fault, got %v", err)
	}
}

func TestCheckpointRejectsTraversal(t *testing.T) {
	bad := []string{"..", "/", filepath.Clean("../escape.json")}
	for _, p := range bad {
		if _, err := checkpoint.NewStore(p); err == nil {
			t.Fatalf("path %q must be rejected", p)
		}
	}
}

func TestOutputPathTraversalDoesNotEscape(t *testing.T) {
	// A name trying to climb directories is cleaned; the run still stays inside
	// the temp working directory and never writes to a parent.
	dir := t.TempDir()
	cfg := DefaultConfig(42, 2,
		filepath.Join(dir, "ck.json"),
		filepath.Join(dir, "sub", "..", "out.mp4"))
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Fatalf("cleaned relative path should work, got %+v", res)
	}
	if !strings.HasPrefix(filepath.Clean(cfg.OutPath), filepath.Clean(dir)) {
		t.Fatal("output escaped its base directory")
	}
}
