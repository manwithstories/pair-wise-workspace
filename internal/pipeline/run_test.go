package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"archive/internal/fault"
	"archive/internal/fixture"
)

// newArchive applies the "not applicable" sentinel (-1) to options the
// test left at zero.
func newArchive(t *testing.T, opt fixture.Options) string {
	t.Helper()
	opt.TruncateAt = orNone(opt.TruncateAt)
	opt.MissingAt = orNone(opt.MissingAt)
	opt.DuplicateAt = orNone(opt.DuplicateAt)
	opt.ConflictAt = orNone(opt.ConflictAt)
	root := t.TempDir()
	if _, err := fixture.Generate(root, opt); err != nil {
		t.Fatal(err)
	}
	return root
}

func orNone(v int) int {
	if v == 0 {
		return -1
	}
	return v
}

func run(t *testing.T, root string, interrupt int) Result {
	t.Helper()
	res, err := Run(Config{Root: root, Manifest: "media.m3u8", InterruptAfter: interrupt})
	if err != nil && fault.LevelOf(err) == fault.Fatal {
		t.Fatalf("run: %v", err)
	}
	return res
}

func TestCleanRunCommitsEverything(t *testing.T) {
	root := newArchive(t, fixture.Options{Segments: 6, Wrap: false})
	res := run(t, root, -1)
	if res.Exit != ExitOK || res.Committed != 6 || res.Pending != 0 || res.Verified != 0 {
		t.Fatalf("result = %+v", res)
	}
	// A second run must verify, not recommit, and improve nothing else.
	res2 := run(t, root, -1)
	if res2.Committed != 0 || res2.Verified != 6 || res2.Exit != ExitOK {
		t.Fatalf("second run = %+v", res2)
	}
	if got, want := DigestEntries(res2.Entries), DigestEntries(res.Entries); got != want {
		t.Fatalf("output digest changed: %s != %s", got, want)
	}
}

func TestInterruptThenResume(t *testing.T) {
	root := newArchive(t, fixture.Options{Segments: 10})
	first := run(t, root, 4)
	if first.Committed != 4 || first.Pending != 6 {
		t.Fatalf("interrupted run = %+v", first)
	}
	second := run(t, root, -1)
	if second.Committed != 6 || second.Verified != 4 || second.Pending != 0 {
		t.Fatalf("resumed run = %+v", second)
	}
	// Re-running the whole job again must not change the digest.
	full := run(t, root, -1)
	if got, want := DigestEntries(full.Entries), DigestEntries(second.Entries); got != want {
		t.Fatalf("digest changed on idempotent rerun")
	}
}

func TestTruncatedSegmentStopsAtFirstBad(t *testing.T) {
	root := newArchive(t, fixture.Options{Segments: 8, TruncateAt: 4})
	res := run(t, root, -1)
	if res.Exit != ExitRecovered {
		t.Fatalf("exit = %d, want %d", res.Exit, ExitRecovered)
	}
	if res.FirstBadIdx != 4 || !strings.Contains(res.FirstBad, "seg-00004") {
		t.Fatalf("first bad = %q index %d", res.FirstBad, res.FirstBadIdx)
	}
	if res.Committed != 4 || res.Pending != 4 {
		t.Fatalf("committed = %d pending = %d, want 4/4", res.Committed, res.Pending)
	}
}

func TestMissingSegmentKeepsConfirmed(t *testing.T) {
	root := newArchive(t, fixture.Options{Segments: 6, MissingAt: 2})
	res := run(t, root, -1)
	if res.Exit != ExitRecovered || res.Committed != 2 {
		t.Fatalf("result = %+v", res)
	}
}

func TestTrackConflictIsFatal(t *testing.T) {
	root := newArchive(t, fixture.Options{Segments: 5, ConflictAt: 2})
	res, err := Run(Config{Root: root, Manifest: "media.m3u8", InterruptAfter: -1})
	if err == nil || fault.LevelOf(err) != fault.Fatal {
		t.Fatalf("conflict must be fatal, got %v", err)
	}
	if res.Committed != 2 {
		t.Fatalf("committed before conflict = %d, want 2", res.Committed)
	}
}

func TestDuplicateReferenceDeduped(t *testing.T) {
	root := newArchive(t, fixture.Options{Segments: 5, DuplicateAt: 2})
	res := run(t, root, -1)
	if res.Exit != ExitOK || res.Duplicates != 1 || res.Unique != 5 {
		t.Fatalf("result = %+v", res)
	}
}

func TestWrappedTimelineIsContinuous(t *testing.T) {
	root := newArchive(t, fixture.Options{Segments: 45, Wrap: true})
	res := run(t, root, -1)
	if res.Exit != ExitOK || res.Committed != 45 {
		t.Fatalf("wrapped run = %+v", res)
	}
}

func TestMissingManifestIsFatal(t *testing.T) {
	root := t.TempDir()
	_, err := Run(Config{Root: root, Manifest: "media.m3u8", InterruptAfter: -1})
	if err == nil || fault.LevelOf(err) != fault.Fatal {
		t.Fatalf("missing manifest must be fatal, got %v", err)
	}
}

func TestUnsafeSegmentPath(t *testing.T) {
	root := t.TempDir()
	pl := "#EXTM3U\n#EXTINF:2,\nok.m4s\n#EXT-X-ENDLIST\n"
	if err := os.WriteFile(filepath.Join(root, "media.m3u8"), []byte(pl), 0o644); err != nil {
		t.Fatal(err)
	}
	// The manifest parser already rejects escapes; this guards the
	// pipeline's own join as defence in depth.
	if _, err := safeSegmentPath(root, "../escape.m4s"); err == nil {
		t.Fatal("escaped path accepted")
	}
	if _, err := safeSegmentPath(root, "media/ok.m4s"); err != nil {
		t.Fatalf("safe path rejected: %v", err)
	}
}
