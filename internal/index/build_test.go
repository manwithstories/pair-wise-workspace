package index

import (
	"os"
	"path/filepath"
	"testing"

	"relay/internal/fixture"
	"relay/internal/ingest"
	"relay/internal/media"
	"relay/internal/output"
	"relay/internal/timeline"
)

func TestIndexOnlyContainsSyncSamples(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "frags"), 0o755); err != nil {
		t.Fatal(err)
	}
	sc := fixture.BuildAllScenario()
	var video []*ingest.Fragment
	for _, in := range sc.Files {
		if in.Truncated || in.TrackID != 101 {
			continue
		}
		f, err := ingest.ParseFragment(in.Name, 0, in.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		video = append(video, f)
	}
	rep := timeline.RepairTrack(101, media.Clock{TrackID: 101, Timescale: 90000}, video)
	if rep.Fault != nil {
		t.Fatal(rep.Fault)
	}
	w := output.NewWriter()
	b := &Builder{}
	for _, rf := range rep.Accepted {
		if _, _, err := w.Write(rf, dir); err != nil {
			t.Fatal(err)
		}
		if err := b.AddFragment(rf, dir); err != nil {
			t.Fatal(err)
		}
	}
	idx := b.Build()
	if len(idx.Entries) != len(rep.Accepted) {
		t.Fatalf("each fragment has exactly one sync sample: got %d entries for %d frags",
			len(idx.Entries), len(rep.Accepted))
	}
	for i, e := range idx.Entries {
		if !e.Sync || e.SampleIndex != 1 {
			t.Fatalf("entry %d must be the first sync sample: %+v", i, e)
		}
		if _, err := filepath.Match("frags/*.m4s", e.FragmentPath); err != nil {
			t.Fatalf("bad fragment path: %v", err)
		}
		if i > 0 && idx.Entries[i-1].GlobalDTS > e.GlobalDTS {
			t.Fatal("index must be sorted by global DTS")
		}
	}
	// Every entry must resolve via Lookup.
	if _, ok := idx.Lookup(101, rep.Accepted[0].NewSequence, 1); !ok {
		t.Fatal("lookup failed for a known keyframe")
	}
}

func TestIndexFileDeterministic(t *testing.T) {
	dir := t.TempDir()
	idx := &Index{Entries: []Entry{
		{TrackID: 1, Sequence: 2, GlobalDTS: 6000, Sync: true},
		{TrackID: 1, Sequence: 1, GlobalDTS: 3000, Sync: true},
	}}
	path := filepath.Join(dir, "index.json")
	if err := Write(idx, path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Entries) != 2 {
		t.Fatal("load mismatch")
	}
}
