package txn

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediarun/internal/clock"
	"mediarun/internal/model"
	"mediarun/internal/verify"
)

func evPayload(name string, start, end clock.Tick) *SegmentPayload {
	return NewSegmentPayload(name, start, end,
		[]model.Event{{Kind: model.KindFrame, TrackID: 1, Start: start, Duration: end - start, Sequence: 1}},
		nil)
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Segments) != 0 || m.Version != CurrentManifestVersion {
		t.Fatalf("empty manifest: %+v", m)
	}
	m.Segments = []SegmentCommit{{Name: "000", SHA256: strings.Repeat("a", 64), Events: 1}}
	if err := SaveManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	back, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Segments) != 1 || back.Segments[0].Name != "000" {
		t.Fatalf("loaded: %+v", back.Segments)
	}
}

func TestManifestValidate(t *testing.T) {
	cases := []struct {
		name string
		m    Manifest
	}{
		{"empty name", Manifest{Segments: []SegmentCommit{{SHA256: strings.Repeat("a", 64)}}}},
		{"short hash", Manifest{Segments: []SegmentCommit{{Name: "a", SHA256: "abc"}}}},
		{"duplicate", Manifest{Segments: []SegmentCommit{
			{Name: "a", SHA256: strings.Repeat("a", 64)},
			{Name: "a", SHA256: strings.Repeat("b", 64)},
		}}},
		{"negative counts", Manifest{Segments: []SegmentCommit{{Name: "a", SHA256: strings.Repeat("a", 64), Events: -1}}}},
		{"overlap", Manifest{Segments: []SegmentCommit{
			{Name: "a", SHA256: strings.Repeat("a", 64), Start: 0, End: 100},
			{Name: "b", SHA256: strings.Repeat("b", 64), Start: 50, End: 200},
		}}},
	}
	for _, c := range cases {
		c.m.Version = CurrentManifestVersion
		if err := c.m.Validate(); !errors.Is(err, model.ErrCorruptIndex) {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	good := Manifest{Version: CurrentManifestVersion, Segments: []SegmentCommit{
		{Name: "a", SHA256: strings.Repeat("a", 64), Start: 0, End: 100},
		{Name: "b", SHA256: strings.Repeat("b", 64), Start: 100, End: 200},
	}}
	if err := good.Validate(); err != nil {
		t.Fatalf("good: %v", err)
	}
}

func TestLoadManifestCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := manifestPath(dir)
	os.WriteFile(path, []byte("{nope"), 0o644)
	if _, err := LoadManifest(dir); !errors.Is(err, model.ErrCorruptIndex) {
		t.Fatalf("garbage manifest: %v", err)
	}
	os.WriteFile(path, []byte(`{"version":9,"segments":[]}`), 0o644)
	if _, err := LoadManifest(dir); !errors.Is(err, model.ErrCorruptIndex) {
		t.Fatalf("stale version: %v", err)
	}
}

func TestCommitRunLifecycle(t *testing.T) {
	dir := t.TempDir()
	run, err := NewCommitRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	if err := run.Begin("compose"); err != nil {
		t.Fatal(err)
	}
	p := evPayload("000", 0, 90000)
	seg, err := run.StageSegment(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(seg.SHA256) != 64 || seg.Events != 1 {
		t.Fatalf("seg: %+v", seg)
	}
	if err := run.CommitSegment(seg); err != nil {
		t.Fatal(err)
	}
	// Idempotent replay: same segment, same hash, no error.
	if err := run.CommitSegment(seg); err != nil {
		t.Fatalf("replay: %v", err)
	}
	// A changed hash for a committed name is rejected.
	bad := seg
	bad.SHA256 = strings.Repeat("c", 64)
	if err := run.CommitSegment(bad); !errors.Is(err, model.ErrCommit) {
		t.Fatalf("hash change: %v", err)
	}

	// The independent verifier rereads the file.
	report, err := VerifyManifest(dir, run.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !report.AllOK() || report.Matched() != 1 {
		t.Fatalf("report: %+v", report)
	}
}

func TestCommittedHashStable(t *testing.T) {
	dir := t.TempDir()
	run, _ := NewCommitRun(dir)
	seg, err := run.StageSegment(evPayload("000", 0, 90000))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.CommitSegment(seg); err != nil {
		t.Fatal(err)
	}
	before, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A second, unrelated stage/rollback must not touch committed hashes.
	if _, err := run.StageSegment(evPayload("001", 90000, 180000)); err != nil {
		t.Fatal(err)
	}
	if err := run.Rollback("compose", "simulated failure"); err != nil {
		t.Fatal(err)
	}
	after, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !CompareHashes(before, after) {
		t.Fatal("committed hash changed after rollback")
	}
	if len(after.Segments) != 1 || after.Stages["compose"] != "rolled-back" {
		t.Fatalf("manifest: %+v", after)
	}
}

func TestWriteSegmentAndRead(t *testing.T) {
	dir := t.TempDir()
	payload := []byte(`{"x":1}`)
	hash, err := WriteSegment(dir, "abc", payload)
	if err != nil {
		t.Fatal(err)
	}
	if hash != verify.BytesHash(payload) {
		t.Fatalf("hash %s", hash)
	}
	data, got, err := ReadSegment(dir, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(payload) || got != hash {
		t.Fatalf("read back: %s %s", data, got)
	}
	if _, _, err := ReadSegment(dir, "missing"); err == nil {
		t.Fatal("missing segment read")
	}
	if _, err := WriteSegment(dir, "", payload); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("empty name: %v", err)
	}
}

func TestRemoveSegmentsNotIn(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"000", "001", "002"} {
		if _, err := WriteSegment(dir, n, []byte(n)); err != nil {
			t.Fatal(err)
		}
	}
	// A non-segment file must survive.
	os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x"), 0o644)
	m := &Manifest{Version: CurrentManifestVersion, Segments: []SegmentCommit{{Name: "001", SHA256: strings.Repeat("a", 64)}}}
	if err := RemoveSegmentsNotIn(dir, m); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"000", "002"} {
		if _, err := os.Stat(filepath.Join(dir, SegmentFile(n))); !os.IsNotExist(err) {
			t.Fatalf("segment %s survived", n)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, SegmentFile("001"))); err != nil {
		t.Fatal("kept segment removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "keep.txt")); err != nil {
		t.Fatal("unrelated file removed")
	}
}
