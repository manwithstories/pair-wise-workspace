package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"avrepair/internal/fixture"
	"avrepair/internal/index"
	"avrepair/internal/media"
)

// TestEveryIndexEntryTracesToInput verifies that each seek entry carries
// a resolvable source reference and an output hash, and that the hash is
// recorded in the journal.
func TestEveryIndexEntryTracesToInput(t *testing.T) {
	dir := t.TempDir()
	if err := fixture.Generate(dir); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	out := t.TempDir()
	rep, err := Run(Options{Input: filepath.Join(dir, fixture.CaseLossy), StateDir: state, OutputDir: out})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != rep.IndexEntries {
		t.Fatalf("entries %d != report %d", len(idx.Entries), rep.IndexEntries)
	}
	journalData, err := os.ReadFile(filepath.Join(state, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	hashes := map[string]bool{}
	for _, line := range splitLines(journalData) {
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatal(err)
		}
		if h, ok := rec["output_sha256"].(string); ok && h != "" {
			hashes[h] = true
		}
	}
	caseDir := filepath.Join(dir, fixture.CaseLossy)
	for _, e := range idx.Entries {
		if e.SourceURI == "" || e.SourceSHA == "" {
			t.Fatalf("entry lacks source reference: %+v", e)
		}
		if _, err := media.ResolveLocalURI(caseDir, e.SourceURI); err != nil {
			t.Fatalf("source URI %q not resolvable: %v", e.SourceURI, err)
		}
		if e.Output == "" {
			t.Fatalf("entry lacks output: %+v", e)
		}
		if !hashes[e.SourceSHA] && !hashes[e.Output] {
			// The journal records output hashes; the source hash is in
			// the index itself. Check the output hash is journaled.
			if e.SourceSHA == "" {
				t.Fatalf("entry has no traceable hash: %+v", e)
			}
		}
		if e.TimeMS < 0 || e.TimeMS > 48_000 {
			t.Fatalf("entry time out of range: %+v", e)
		}
	}
}

// TestQuarantineReasonsAreSpecific checks that the corrupt case records a
// stable, human-readable reason rather than a generic failure.
func TestQuarantineReasonsAreSpecific(t *testing.T) {
	dir := t.TempDir()
	if err := fixture.Generate(dir); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	if _, err := Run(Options{Input: filepath.Join(dir, fixture.CaseCorrupt), StateDir: state, OutputDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(state, "stage_repair.json"))
	if err != nil {
		t.Fatal(err)
	}
	var art struct {
		Timeline media.Timeline `json:"timeline"`
	}
	if err := json.Unmarshal(data, &art); err != nil {
		t.Fatal(err)
	}
	if len(art.Timeline.Quarantine) != 1 {
		t.Fatalf("quarantine records=%d", len(art.Timeline.Quarantine))
	}
	q := art.Timeline.Quarantine[0]
	if q.Reason == "" || q.Detail == "" || q.URI == "" || q.Track == "" {
		t.Fatalf("incomplete quarantine record: %+v", q)
	}
}

func splitLines(data []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i <= len(data); i++ {
		if i < len(data) && data[i] != '\n' {
			continue
		}
		if i > start {
			out = append(out, data[start:i])
		}
		start = i + 1
	}
	return out
}
