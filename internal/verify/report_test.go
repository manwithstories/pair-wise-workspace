package verify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/audio"
	"mediad/internal/fingerprint"
	"mediad/internal/model"
	"mediad/internal/segment"
)

func sampleInputs() Inputs {
	return Inputs{
		Cuts: []segment.Cut{
			{Track: model.TrackField, Index: 1},
			{Track: model.TrackStudio, Index: 0},
			{Track: model.TrackStudio, Index: 1},
		},
		Silence: []audio.Interval{
			{StartMS: 5000, EndMS: 5300},
			{StartMS: 1000, EndMS: 1500},
		},
		Duplicates: []fingerprint.DuplicateGroup{
			{DigestHex: "bbb", Sources: []fingerprint.Source{{Track: model.TrackVoice, Index: 2}}},
			{DigestHex: "aaa", Sources: []fingerprint.Source{{Track: model.TrackField, Index: 0}}},
		},
		Bad: []BadOffset{
			{Track: "voiceover", Segment: 4, Offset: 2048, Recoverable: true, Op: "walk", Reason: "truncated segment"},
			{Track: "studio", Segment: 1, Offset: 512, Recoverable: false, Op: "walk", Reason: "bad segment payload"},
		},
		Blocked:   "studio#1",
		ResumeMS:  2000,
		ElapsedMS: 42,
	}
}

func TestBuild_DeterministicOrderingAndFirstBad(t *testing.T) {
	r := Build(sampleInputs())
	if r.Segments != 3 {
		t.Fatalf("segments = %d", r.Segments)
	}
	if r.Cuts[0].Track != "field" || r.Cuts[1].Count != 2 || r.Cuts[1].Track != "studio" {
		t.Fatalf("cut counts not sorted: %+v", r.Cuts)
	}
	if r.SilenceMS[0].StartMS != 1000 {
		t.Fatalf("silence must be sorted by start: %+v", r.SilenceMS)
	}
	if r.Duplicates[0].DigestHex != "aaa" {
		t.Fatalf("duplicates must be sorted: %+v", r.Duplicates)
	}
	if r.FirstBadOffset == nil || r.FirstBadOffset.Offset != 512 || r.FirstBadOffset.Track != "studio" {
		t.Fatalf("first bad offset must be the smallest-offset defect: %+v", r.FirstBadOffset)
	}
}

func TestFinalize_RerunsHaveIdenticalSHA256(t *testing.T) {
	r1 := Build(sampleInputs())
	r2 := Build(sampleInputs())
	h1, err := r1.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	h2, err := r2.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	if h1 == "" || h1 != h2 {
		t.Fatalf("reproducible digest required, got %q vs %q", h1, h2)
	}
	// A stable-content change must move the digest; timing fields are
	// deliberately excluded so reruns stay bit-identical.
	in := sampleInputs()
	in.Cuts = append(in.Cuts, segment.Cut{Track: model.TrackVoice, Index: 9})
	r3 := Build(in)
	h3, _ := r3.Finalize()
	if h3 == h1 {
		t.Fatal("changed stable content must change sha256")
	}
	// Changing only volatile timing must keep the digest stable.
	timed := sampleInputs()
	timed.ElapsedMS = 98765
	timed.ResumeMS = 4321
	if h, _ := Build(timed).Finalize(); h != h1 {
		t.Fatalf("timing-only change must not move sha256, got %s want %s", h, h1)
	}
}

func TestWriteReadRecheckRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "report.json")
	r := Build(sampleInputs())
	if err := r.WriteTo(path); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if err := Recheck(path); err != nil {
		t.Fatalf("fresh report must recheck: %v", err)
	}
	loaded, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SHA256 != r.SHA256 || loaded.BlockedNode != "studio#1" {
		t.Fatalf("round trip mismatch: %+v", loaded)
	}
}

func TestRecheck_DetectsTampering(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	r := Build(sampleInputs())
	if err := r.WriteTo(path); err != nil {
		t.Fatal(err)
	}
	// Tamper with stable content but keep the old digest field: marshal
	// directly, bypassing Finalize which would recompute sha256.
	loaded, _ := Read(path)
	loaded.Segments = loaded.Segments + 1
	data, err := json.MarshalIndent(loaded, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Recheck(path); err == nil {
		t.Fatal("tampered report must fail recheck")
	}
}

func TestBuild_NoDefectsLeavesFirstBadNil(t *testing.T) {
	in := sampleInputs()
	in.Bad = nil
	r := Build(in)
	if r.FirstBadOffset != nil {
		t.Fatalf("clean input must have nil bad offset, got %+v", r.FirstBadOffset)
	}
}
