package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func guards(sha string) Guards {
	return Guards{
		SourceSHA256:   sha,
		Adapter:        "mp4",
		AdapterVersion: 1,
		ConfigDigest:   ConfigDigest("keep_payload=false", "max=20000000"),
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	j, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := &State{
		AssetID: "A1", TrackID: 1, CommittedThrough: 99, CommitCount: 100,
		SourceSHA256: "abc", Adapter: "mp4", AdapterVersion: 1, ConfigDigest: "cfg",
	}
	if err := j.Save(st); err != nil {
		t.Fatal(err)
	}
	got, q, err := j.Load("A1", 1, Guards{SourceSHA256: "abc", Adapter: "mp4", AdapterVersion: 1, ConfigDigest: "cfg"})
	if err != nil || q {
		t.Fatalf("load err=%v q=%v", err, q)
	}
	if got.CommittedThrough != 99 || got.CommitCount != 100 {
		t.Fatalf("state = %+v", got)
	}
}

func TestLoadMissing(t *testing.T) {
	j, _ := Open(t.TempDir())
	got, q, err := j.Load("nope", 1, guards("x"))
	if err != nil || q || got != nil {
		t.Fatalf("got=%v q=%v err=%v", got, q, err)
	}
}

func TestGuardMismatchQuarantines(t *testing.T) {
	cases := []struct {
		name string
		g    Guards
	}{
		{"source changed", guards("different")},
		{"adapter changed", Guards{SourceSHA256: "abc", Adapter: "rtp", AdapterVersion: 1, ConfigDigest: "cfg"}},
		{"version changed", Guards{SourceSHA256: "abc", Adapter: "mp4", AdapterVersion: 2, ConfigDigest: "cfg"}},
		{"config changed", Guards{SourceSHA256: "abc", Adapter: "mp4", AdapterVersion: 1, ConfigDigest: "other"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j, _ := Open(t.TempDir())
			st := &State{AssetID: "A1", TrackID: 1, CommittedThrough: 5,
				SourceSHA256: "abc", Adapter: "mp4", AdapterVersion: 1, ConfigDigest: "cfg"}
			if err := j.Save(st); err != nil {
				t.Fatal(err)
			}
			got, q, err := j.Load("A1", 1, tc.g)
			if err != nil {
				t.Fatal(err)
			}
			if got != nil || !q {
				t.Fatalf("expected quarantine, got=%v q=%v", got, q)
			}
			list, err := j.Quarantined()
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 1 {
				t.Fatalf("quarantine list = %v", list)
			}
			// Original checkpoint must no longer be loadable.
			got2, _, _ := j.Load("A1", 1, tc.g)
			if got2 != nil {
				t.Fatal("quarantined checkpoint still loadable")
			}
		})
	}
}

func TestCorruptCheckpointQuarantined(t *testing.T) {
	j, _ := Open(t.TempDir())
	path := filepath.Join(j.Dir(), fileName("A1", 1))
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, q, err := j.Load("A1", 1, guards("abc"))
	if !q || err == nil {
		t.Fatalf("q=%v err=%v", q, err)
	}
	list, _ := j.Quarantined()
	if len(list) != 1 {
		t.Fatalf("quarantine = %v", list)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	j, _ := Open(dir)
	// Write a first checkpoint then a second; no temp files should remain and
	// exactly one JSON should exist.
	for i := int64(0); i < 5; i++ {
		st := &State{AssetID: "A1", TrackID: 1, CommittedThrough: i,
			SourceSHA256: "abc", Adapter: "mp4", AdapterVersion: 1, ConfigDigest: "cfg"}
		if err := j.Save(st); err != nil {
			t.Fatal(err)
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	jsons := 0
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) != ".json" {
			t.Fatalf("stray temp file %q", e.Name())
		}
		jsons++
	}
	if jsons != 1 {
		t.Fatalf("json files = %d", jsons)
	}
}

func TestPlanQuarantineFlags(t *testing.T) {
	j, _ := Open(t.TempDir())
	if err := j.Save(&State{AssetID: "A1", TrackID: 1, CommittedThrough: 9, CommitCount: 10,
		SourceSHA256: "abc", Adapter: "mp4", AdapterVersion: 1, ConfigDigest: "cfg"}); err != nil {
		t.Fatal(err)
	}
	plans, err := j.Plan("A1", []uint32{1, 2},
		Guards{SourceSHA256: "abc", Adapter: "mp4", AdapterVersion: 1, ConfigDigest: "cfg"})
	if err != nil {
		t.Fatal(err)
	}
	if plans[0].ResumeAfter != 10 || plans[0].Committed != 10 {
		t.Fatalf("track 1 plan = %+v", plans[0])
	}
	if plans[1].ResumeAfter != 0 {
		t.Fatalf("track 2 should start at 0: %+v", plans[1])
	}
}

func TestParseTrackID(t *testing.T) {
	id, err := ParseTrackID("asset-A1__track-7.json")
	if err != nil || id != 7 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if _, err := ParseTrackID("nonsense"); err == nil {
		t.Fatal("want error")
	}
}
