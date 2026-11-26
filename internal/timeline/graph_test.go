package timeline

import (
	"testing"

	"relay/internal/fixture"
	"relay/internal/ingest"
	"relay/internal/media"
)

func parseAll(t *testing.T, sc *fixture.Scenario) map[uint32][]*ingest.Fragment {
	t.Helper()
	out := map[uint32][]*ingest.Fragment{}
	for _, in := range sc.Files {
		if in.Truncated {
			continue // parse faults are exercised in the ingest tests
		}
		f, err := ingest.ParseFragment(in.Name, 0, in.Bytes)
		if err != nil {
			t.Fatalf("unexpected parse fault for %s: %v", in.Name, err)
		}
		out[f.TrackID] = append(out[f.TrackID], f)
	}
	return out
}

func TestRepairGapAndRollback(t *testing.T) {
	sc := fixture.BuildAllScenario()
	byTrack := parseAll(t, sc)

	video := RepairTrack(101, media.Clock{TrackID: 101, Timescale: 90000}, byTrack[101])
	if video.Fault != nil {
		t.Fatalf("video repair failed: %v", video.Fault)
	}
	if len(video.Accepted) != 12 {
		t.Fatalf("expected 12 accepted video fragments, got %d", len(video.Accepted))
	}
	if len(video.GapsFilled) != 1 || video.GapsFilled[0] != 5 {
		t.Fatalf("expected single gap at seq 5, got %v", video.GapsFilled)
	}
	for i, rf := range video.Accepted {
		if rf.NewSequence != uint32(i+1) {
			t.Fatalf("renumbering not contiguous at %d: %d", i, rf.NewSequence)
		}
		if i > 0 && rf.GlobalDTS < video.Accepted[i-1].GlobalDTS {
			t.Fatalf("global DTS went backwards at %d", i)
		}
	}
}

func TestRepairDuplicateAndConflict(t *testing.T) {
	sc := fixture.BuildAllScenario()
	byTrack := parseAll(t, sc)
	dub := RepairTrack(301, media.Clock{TrackID: 301, Timescale: 48000}, byTrack[301])
	if dub.Fault != nil {
		t.Fatalf("duplicate must be tolerated: %v", dub.Fault)
	}
	if dub.Deduped != 1 {
		t.Fatalf("expected one dedup, got %d", dub.Deduped)
	}
	if len(dub.Accepted) != 5 {
		t.Fatalf("expected 5 accepted dub fragments, got %d", len(dub.Accepted))
	}

	// Same sequence, different bytes -> conflict isolates the track.
	conflict := append([]*ingest.Fragment(nil), byTrack[301]...)
	raw := fixture.BuildFragment(fixture.FragmentSpec{
		TrackID: 301, Sequence: 3, Tfdt: 99999,
		Samples: []fixture.SampleSpec{
			{Duration: 1024, Payload: fixture.SamplePayload(9, 9, 9, 16), Sync: true},
		},
	})
	f, err := ingest.ParseFragment("conflict", 0, raw)
	if err != nil {
		t.Fatal(err)
	}
	conflict = append(conflict, f)
	bad := RepairTrack(301, media.Clock{TrackID: 301, Timescale: 48000}, conflict)
	if bad.Fault == nil || bad.Fault.Code != media.FaultConflict {
		t.Fatalf("expected sequence conflict, got %v", bad.Fault)
	}
	if len(bad.Accepted) != 0 {
		t.Fatal("conflicted track must not publish fragments")
	}
}

func TestGraphCycleAndMissingDep(t *testing.T) {
	g := NewGraph([]TrackSpec{
		{ID: 1}, {ID: 2, DependsOn: []uint32{1}},
		{ID: 404, DependsOn: []uint32{404}}, // self-loop
	})
	if g.CycleRejected() != 1 {
		t.Fatalf("expected one rejected cycle, got %d", g.CycleRejected())
	}
	n, _ := g.Node(404)
	if n.Reason == nil || n.Reason.Code != media.FaultGraphCycle {
		t.Fatalf("self-loop must be a cycle fault, got %v", n.Reason)
	}
	if order := g.Order(); len(order) != 2 {
		t.Fatalf("cycle member must be excluded from order, got %v", order)
	}

	missing := NewGraph([]TrackSpec{{ID: 7, DependsOn: []uint32{99}}})
	m, _ := missing.Node(7)
	if m.Reason == nil || m.Reason.Code != media.FaultMissingDep {
		t.Fatalf("expected missing dependency fault, got %v", m.Reason)
	}
}

func TestFailurePropagationBlocksDependentsOnly(t *testing.T) {
	g := NewGraph([]TrackSpec{
		{ID: 1}, {ID: 2, DependsOn: []uint32{1}},
		{ID: 3, DependsOn: []uint32{1, 2}}, {ID: 4}, // 4 is independent
	})
	g.MarkPublished(1, media.NewFault(media.FaultUnrepairable, 1, 0, 0, "boom"))
	g.MarkPublished(2, nil)
	g.MarkPublished(3, nil)
	g.MarkPublished(4, nil)
	g.PropagateFailures()

	if n, _ := g.Node(2); n.Reason == nil || n.Reason.Code != media.FaultDepFailed {
		t.Fatalf("track 2 must be blocked by failed parent, got %v", n.Reason)
	}
	if n, _ := g.Node(3); n.Reason == nil || n.Reason.Code != media.FaultDepFailed {
		t.Fatalf("track 3 must be transitively blocked, got %v", n.Reason)
	}
	if n, _ := g.Node(4); n.Reason != nil {
		t.Fatalf("independent track 4 must keep processing, got %v", n.Reason)
	}
}
