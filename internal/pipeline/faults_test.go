package pipeline

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"avrepair/internal/fixture"
	"avrepair/internal/index"
	"avrepair/internal/media"
	"avrepair/internal/recovery"
	"avrepair/internal/segment"
)

func copyCase(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestInvalidMPDTimescaleIsRejected(t *testing.T) {
	src := t.TempDir()
	if err := fixture.Generate(src); err != nil {
		t.Fatal(err)
	}
	dir := copyCase(t, filepath.Join(src, fixture.CaseNormal))
	mpd := filepath.Join(dir, "presentation.mpd")
	data, err := os.ReadFile(mpd)
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(data), `timescale="90000"`, `timescale="-5"`, 1)
	if bad == string(data) {
		t.Fatal("no timescale attribute to corrupt")
	}
	if err := os.WriteFile(mpd, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Run(Options{Input: dir, StateDir: t.TempDir(), OutputDir: t.TempDir()})
	if err == nil || !errors.Is(err, media.ErrInvalidTimescale) {
		t.Fatalf("want invalid timescale error, got %v", err)
	}
}

func TestPTSWrapStaysMonotonic(t *testing.T) {
	dir := t.TempDir()
	if err := fixture.Generate(dir); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(Options{Input: filepath.Join(dir, fixture.CaseNormal), StateDir: t.TempDir(), OutputDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// A wrap that was not repaired would produce a negative or huge span.
	if rep.IndexEntries != 96 || rep.Quarantined != 0 {
		t.Fatalf("wrap handling lost segments: %+v", rep)
	}
}

func TestUnrecoverableLossQuarantinesOnlyAffectedSegment(t *testing.T) {
	dir := t.TempDir()
	if err := fixture.Generate(dir); err != nil {
		t.Fatal(err)
	}
	caseDir := copyCase(t, filepath.Join(dir, fixture.CaseNormal))
	segPath := filepath.Join(caseDir, "hls", "video-00003.ts")
	raw, err := os.ReadFile(segPath)
	if err != nil {
		t.Fatal(err)
	}
	pkts, err := segment.DecodePackets(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Drop two data packets from the same parity group: unrecoverable.
	var kept []segment.Packet
	dropped := 0
	for _, p := range pkts {
		if !p.Parity && dropped < 2 && p.Seq < 2 {
			dropped++
			continue
		}
		kept = append(kept, p)
	}
	if dropped != 2 {
		t.Fatal("failed to inject double loss")
	}
	if err := os.WriteFile(segPath, segment.EncodePackets(kept), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	rep, err := Run(Options{Input: caseDir, StateDir: t.TempDir(), OutputDir: out})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Quarantined != 1 || rep.Unrecovered == 0 {
		t.Fatalf("unrecoverable loss not isolated: %+v", rep)
	}
	data, err := os.ReadFile(filepath.Join(out, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range idx.Entries {
		if strings.HasSuffix(e.SourceURI, "video-00003.ts") {
			t.Fatal("index references the unrecoverable segment")
		}
	}
	if rep.IndexEntries != 95 {
		t.Fatalf("seek_index_entries=%d, want 95", rep.IndexEntries)
	}
}

func TestCorruptCommittedIndexIsRebuiltOnResume(t *testing.T) {
	dir := t.TempDir()
	if err := fixture.Generate(dir); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	state := t.TempDir()
	in := filepath.Join(dir, fixture.CaseNormal)
	if _, err := Run(Options{Input: in, StateDir: state, OutputDir: out}); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(out, "index.json")
	good, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-commit: a truncated index on disk.
	if err := os.WriteFile(indexPath, good[:len(good)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(Options{Input: in, StateDir: state, OutputDir: out, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.IndexEntries != 96 {
		t.Fatalf("index was not rebuilt: entries=%d", rep.IndexEntries)
	}
	rebuilt, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(rebuilt) != string(good) {
		t.Fatal("rebuilt index differs from the original")
	}
}

func TestCheckpointArtifactTamperingFailsResume(t *testing.T) {
	dir := t.TempDir()
	if err := fixture.Generate(dir); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	state := t.TempDir()
	in := filepath.Join(dir, fixture.CaseNormal)
	if _, err := Run(Options{Input: in, StateDir: state, OutputDir: out}); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(state, "stage_repair.json")
	if err := os.WriteFile(artifact, []byte(`{"tampered":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(Options{Input: in, StateDir: state, OutputDir: out, Resume: true})
	if err != nil {
		t.Fatalf("tampered artifact must fall back to recompute: %v", err)
	}
	for _, name := range rep.ReusedNames {
		if name == StageRepair {
			t.Fatalf("tampered repair artifact was reused: %+v", rep.ReusedNames)
		}
	}
}

func TestRecoveryCountersMatchJournal(t *testing.T) {
	dir := t.TempDir()
	if err := fixture.Generate(dir); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	rep, err := Run(Options{Input: filepath.Join(dir, fixture.CaseLossy), StateDir: state, OutputDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.RecoveredPackets == 0 {
		t.Fatal("lossy case reported no recovered packets")
	}
	data, err := os.ReadFile(filepath.Join(state, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		if rec["stage"] == "repair" && rec["output_sha256"] != nil {
			count++
		}
	}
	if count != rep.SegmentsValid {
		t.Fatalf("journal output records=%d, valid segments=%d", count, rep.SegmentsValid)
	}
	var _ = recovery.GroupSize
}
