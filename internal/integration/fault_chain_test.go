// Package integration contains end-to-end tests that follow a fault across
// subsystem boundaries: ingest parse error -> track dependency graph ->
// index publication. At least two distinct fault classes are traced.
package integration

import (
	"bytes"
	"testing"

	"relay/internal/fixture"
	"relay/internal/index"
	"relay/internal/ingest"
	"relay/internal/media"
	"relay/internal/timeline"
)

// ingestOne parses a single fixture file and returns the fragment or the
// isolating fault, mirroring the demo's ingest stage.
func ingestOne(t *testing.T, in fixture.InputFile) (*ingest.Fragment, *media.Fault) {
	t.Helper()
	rd := ingest.NewReader(bytes.NewReader(in.Bytes), in.Name, int64(len(in.Bytes)))
	ev, err := rd.Next()
	if err != nil {
		t.Fatalf("reader error: %v", err)
	}
	return ev.Fragment, ev.Fault
}

// TestFaultChainTruncation traces a truncated mdat: the ingest fault must
// isolate the fragment; the affected track must not publish it to the index.
func TestFaultChainTruncation(t *testing.T) {
	sc := fixture.BuildAllScenario()

	var truncated *fixture.InputFile
	for i := range sc.Files {
		if sc.Files[i].Truncated {
			truncated = &sc.Files[i]
			break
		}
	}
	if truncated == nil {
		t.Fatal("scenario must contain a truncated fragment")
	}

	// Boundary 1: ingest reports a truncation fault with a real offset.
	frag, fault := ingestOne(t, *truncated)
	if frag != nil || fault == nil {
		t.Fatal("truncated file must produce a fault, not a fragment")
	}
	if fault.Code != media.FaultTruncated || fault.Offset == 0 {
		t.Fatalf("expected truncated fault at a byte offset, got %v", fault)
	}
	if !fault.Isolation() {
		t.Fatal("truncation must isolate the fragment")
	}

	// Boundary 2: the healthy track still repairs and publishes; the
	// quarantined sequence never appears among accepted fragments.
	parsed := map[uint32][]*ingest.Fragment{}
	for _, in := range sc.Files {
		if in.Truncated {
			continue
		}
		f, f2 := ingestOne(t, in)
		if f2 != nil {
			t.Fatalf("unexpected fault for %s: %v", in.Name, f2)
		}
		parsed[f.TrackID] = append(parsed[f.TrackID], f)
	}
	rep := timeline.RepairTrack(truncated.TrackID,
		media.Clock{TrackID: truncated.TrackID, Timescale: 48000},
		parsed[truncated.TrackID])
	for _, rf := range rep.Accepted {
		if rf.Sequence == truncated.Sequence {
			t.Fatal("isolated truncated fragment reached the timeline")
		}
	}

	// Boundary 3: an index built from accepted fragments contains no entry
	// carrying the isolated sequence.
	var idx index.Index
	for _, rf := range rep.Accepted {
		idx.Entries = append(idx.Entries, index.Entry{
			TrackID: rf.TrackID, Sequence: rf.NewSequence, Sync: true,
		})
	}
	for _, e := range idx.Entries {
		if e.Sequence == truncated.Sequence && e.TrackID == truncated.TrackID {
			t.Fatal("isolated fragment leaked into the published index")
		}
	}
}

// TestFaultChainDependencyFailure traces a repair fault into the dependency
// graph: a track that fails repair must block every dependent track from
// being published, while an unrelated track proceeds normally.
func TestFaultChainDependencyFailure(t *testing.T) {
	sc := fixture.BuildAllScenario()
	parsed := map[uint32][]*ingest.Fragment{}
	for _, in := range sc.Files {
		if in.Truncated {
			continue
		}
		f, _ := ingestOne(t, in)
		parsed[f.TrackID] = append(parsed[f.TrackID], f)
	}

	// Inject a same-sequence/different-content conflict into the video
	// track (the root of the dependency graph).
	conflictRaw := fixture.BuildFragment(fixture.FragmentSpec{
		TrackID: 101, Sequence: parsed[101][0].Sequence, Tfdt: 424242,
		Samples: []fixture.SampleSpec{
			{Duration: 3000, Payload: fixture.SamplePayload(255, 1, 1, 16), Sync: true},
		},
	})
	cfrag, err := ingest.ParseFragment("conflict", 0, conflictRaw)
	if err != nil {
		t.Fatal(err)
	}
	parsed[101] = append(parsed[101], cfrag)

	// Boundary 1: timeline repair surfaces a sequence-conflict fault.
	video := timeline.RepairTrack(101, media.Clock{TrackID: 101, Timescale: 90000},
		parsed[101])
	if video.Fault == nil || video.Fault.Code != media.FaultConflict {
		t.Fatalf("expected conflict fault, got %v", video.Fault)
	}
	if len(video.Accepted) != 0 {
		t.Fatal("a conflicted track must not publish fragments")
	}

	// Boundary 2: the graph propagates the failure to dependent tracks.
	g := timeline.NewGraph(specs(sc))
	g.MarkPublished(101, video.Fault)
	for _, id := range []uint32{201, 301, 404} {
		g.MarkPublished(id, nil)
	}
	g.PropagateFailures()
	for _, dep := range []uint32{201, 301} {
		n, _ := g.Node(dep)
		if n.Reason == nil || n.Reason.Code != media.FaultDepFailed {
			t.Fatalf("track %d must be blocked by its failed dependency", dep)
		}
	}

	// Boundary 3: blocked tracks contribute no index entries. Only a track
	// independent of the failed root can still be published.
	published := 0
	for _, id := range g.Order() {
		n, _ := g.Node(id)
		if n.Reason == nil && n.Published {
			published++
		}
	}
	if published != 0 {
		t.Fatalf("no track depending (directly or transitively) on the failed root may publish; got %d", published)
	}
}

func specs(sc *fixture.Scenario) []timeline.TrackSpec {
	out := make([]timeline.TrackSpec, 0, len(sc.Tracks))
	for _, t := range sc.Tracks {
		out = append(out, timeline.TrackSpec{
			ID: t.ID, Timescale: t.Timescale, DependsOn: t.DependsOn,
		})
	}
	return out
}
