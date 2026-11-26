package verify

import (
	"context"
	"path/filepath"
	"testing"

	"relay/internal/fixture"
	"relay/internal/index"
	"relay/internal/ingest"
	"relay/internal/media"
	"relay/internal/output"
	"relay/internal/stream"
	"relay/internal/timeline"
)

func harness(t *testing.T, dir string) []*timeline.TrackReport {
	t.Helper()
	sc := fixture.BuildAllScenario()
	byTrack := map[uint32][]*ingest.Fragment{}
	for _, in := range sc.Files {
		if in.Truncated {
			continue
		}
		f, err := ingest.ParseFragment(in.Name, 0, in.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		byTrack[f.TrackID] = append(byTrack[f.TrackID], f)
	}
	reports := []*timeline.TrackReport{
		timeline.RepairTrack(101, media.Clock{TrackID: 101, Timescale: 90000}, byTrack[101]),
		timeline.RepairTrack(201, media.Clock{TrackID: 201, Timescale: 48000}, byTrack[201]),
		timeline.RepairTrack(301, media.Clock{TrackID: 301, Timescale: 48000}, byTrack[301]),
	}
	for _, r := range reports {
		if r.Fault != nil {
			t.Fatalf("repair fault: %v", r.Fault)
		}
	}
	return reports
}

func runFull(t *testing.T, dir string, reports []*timeline.TrackReport, sink stream.Sink) *stream.Pipeline {
	t.Helper()
	p, err := stream.New(stream.Config{OutDir: dir, QueueBytes: 1 << 20}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(context.Background(), reports); err != nil {
		t.Fatalf("full run: %v", err)
	}
	return p
}

func TestRoundTripAndHashes(t *testing.T) {
	dir := t.TempDir()
	reports := harness(t, dir)
	p := runFull(t, dir, reports, output.NewWriter())

	rt, err := RoundTrip(dir, p.Checkpoint())
	if err != nil {
		t.Fatal(err)
	}
	if rt.Failed != 0 || rt.Rate != 100 {
		t.Fatalf("expected 100%% roundtrip, got %d/%d faults=%v",
			rt.Reparsed, rt.Reparsed+rt.Failed, rt.Faults)
	}
	matched, total, fault := Hashes(dir, p.Checkpoint())
	if fault != nil || matched != total {
		t.Fatalf("hash mismatch: %d/%d %v", matched, total, fault)
	}
	if total != 23 {
		t.Fatalf("expected 23 committed healthy fragments, got %d", total)
	}
}

func TestKeyframeReachability(t *testing.T) {
	dir := t.TempDir()
	reports := harness(t, dir)
	runFull(t, dir, reports, output.NewWriter())
	b := &index.Builder{}
	for _, r := range reports {
		for _, rf := range r.Accepted {
			if err := b.AddFragment(rf, dir); err != nil {
				t.Fatal(err)
			}
		}
	}
	idx := b.Build()
	hits, expected, fault := Keyframes(idx, reports)
	if fault != nil {
		t.Fatal(fault)
	}
	// Every healthy fragment contributes exactly one sync sample.
	if hits != expected || hits != 23 {
		t.Fatalf("every accepted fragment has one keyframe: %d/%d", hits, expected)
	}
	// Persist and reload to prove the index survives an independent read.
	path := filepath.Join(dir, "index.json")
	if err := index.Write(idx, path); err != nil {
		t.Fatal(err)
	}
	reloaded, err := index.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	videoHits := 0
	for _, e := range reloaded.Entries {
		if e.TrackID == 101 {
			videoHits++
		}
	}
	if videoHits != 12 {
		t.Fatalf("expected 12 video keyframes, got %d", videoHits)
	}
}

func TestResumeMatchesReference(t *testing.T) {
	refDir := t.TempDir()
	resumeDir := t.TempDir()
	reports := harness(t, refDir)

	ref := runFull(t, refDir, reports, output.NewWriter())
	refHash := ManifestHash(ref.Checkpoint())

	// Interrupted then resumed run in a separate directory.
	p1, err := stream.New(stream.Config{OutDir: resumeDir, QueueBytes: 1 << 20,
		InterruptAfter: 9}, output.NewWriter())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p1.Run(context.Background(), reports); err != stream.ErrInterrupted {
		t.Fatalf("want interrupt, got %v", err)
	}
	if err := p1.Close(); err != nil {
		t.Fatal(err)
	}
	p2, err := stream.New(stream.Config{OutDir: resumeDir, QueueBytes: 1 << 20},
		output.NewWriter())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p2.Run(context.Background(), reports); err != nil {
		t.Fatalf("resume: %v", err)
	}
	match, total, fault := Resume(ref.Checkpoint(), p2.Checkpoint())
	if fault != nil {
		t.Fatal(fault)
	}
	if match != total || total != 23 {
		t.Fatalf("resume mismatch: %d/%d", match, total)
	}
	if ManifestHash(p2.Checkpoint()) != refHash {
		t.Fatal("resumed manifest hash must equal the full reference run")
	}
}
